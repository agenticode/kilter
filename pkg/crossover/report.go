// Human-facing rendering of a crossover report. The three methods here take a
// value receiver, not a pointer: they only read, and Analyze returns a value,
// so Analyze(now, set, opts).Summary() must compile. Everything here is advisory
// text: Summary is what `analyze --fargate` prints, Insight is what /insights
// carries. Neither can become a step — model.Insight is a finding, not an
// action, and this package has no other output type.

package crossover

import (
	"fmt"
	"sort"
	"strings"

	"github.com/agenticode/kilter/pkg/model"
)

// InsightKind is the model.Insight kind this package emits. Advisory only.
const InsightKind = "fargate-crossover"

// Headline is the one-sentence answer: which side wins, by how much, or why
// the question does not apply. It never says Fargate is cheaper while a gate
// blocks — when blocked, the price gap is not part of the sentence at all.
func (r Report) Headline() string {
	switch r.Verdict {
	case VerdictFargate:
		return fmt.Sprintf("Fargate is cheaper for these %d pods: $%.2f/mo vs $%.2f/mo on EC2 nodes (saves $%.2f/mo, %.1f%%)%s",
			r.Fargate.Pods, r.Fargate.MonthlyUSD, r.EC2.MonthlyUSD,
			r.MonthlySavingsUSD, r.SavingsFraction*100, closeNote(r.Close))
	case VerdictEC2:
		return fmt.Sprintf("EC2 nodes are cheaper for these %d pods: $%.2f/mo vs $%.2f/mo on Fargate (saves $%.2f/mo, %.1f%%)%s",
			r.Fargate.Pods, r.EC2.MonthlyUSD, r.Fargate.MonthlyUSD,
			r.MonthlySavingsUSD, r.SavingsFraction*100, closeNote(r.Close))
	case VerdictTie:
		return fmt.Sprintf("Fargate and EC2 nodes cost the same for these %d pods: $%.2f/mo either way",
			r.Fargate.Pods, r.EC2.MonthlyUSD)
	case VerdictFargateBlocked:
		return fmt.Sprintf("Fargate is not an option for these %d pods at any price: %s",
			r.Fargate.Pods, blockedBy(r.Blocks))
	default:
		return fmt.Sprintf("no crossover verdict for these %d pods: neither side could be priced or run them",
			r.Fargate.Pods)
	}
}

func closeNote(close bool) string {
	if close {
		return " — but the gap is inside this model's error bars; do not move on price alone"
	}
	return ""
}

// blockedBy renders the distinct gates that blocked, in AllGates order.
func blockedBy(blocks []Block) string {
	order := make(map[Gate]int, len(AllGates()))
	for i, g := range AllGates() {
		order[g] = i
	}
	seen := make(map[Gate]bool, len(blocks))
	var gates []Gate
	for _, b := range blocks {
		if !seen[b.Gate] {
			seen[b.Gate] = true
			gates = append(gates, b.Gate)
		}
	}
	sort.Slice(gates, func(i, j int) bool { return order[gates[i]] < order[gates[j]] })
	parts := make([]string, 0, len(gates))
	for _, g := range gates {
		parts = append(parts, string(g))
	}
	return strings.Join(parts, ", ")
}

// Summary renders the whole report as plain text, deterministically. This is
// what `kilter analyze --fargate` prints.
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fargate ⇄ EC2 crossover (%s)\n", r.At.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(&b, "  %s\n", r.Headline())

	fmt.Fprintf(&b, "  Fargate  F(P): $%.4f/h  $%.2f/mo  over %d pod(s)%s\n",
		r.Fargate.HourlyUSD, r.Fargate.MonthlyUSD, r.Fargate.Pods, eligibleNote(r.Fargate.Eligible))
	for _, c := range r.Fargate.Configs {
		fmt.Fprintf(&b, "    %-16s × %-4d $%.4f/h\n", c.Config, c.Pods, c.HourlyUSD)
	}
	for _, p := range r.Fargate.Unpriced {
		fmt.Fprintf(&b, "    unpriced: %s (above the Fargate ceiling)\n", p)
	}

	fmt.Fprintf(&b, "  EC2      E(P): $%.4f/h  $%.2f/mo  over %d node(s)%s\n",
		r.EC2.HourlyUSD, r.EC2.MonthlyUSD, r.EC2.Nodes, feasibleNote(r.EC2.Feasible))
	for _, t := range r.EC2.NodeTypes {
		fmt.Fprintf(&b, "    %-16s × %-4d $%.4f/h%s\n", t.Name, t.Nodes, t.HourlyUSD, spotNote(t.Spot))
	}
	if r.EC2.DaemonSetTemplates > 0 {
		fmt.Fprintf(&b, "    %d DaemonSet(s) replicated onto every node\n", r.EC2.DaemonSetTemplates)
	}
	for _, u := range r.EC2.Unschedulable {
		fmt.Fprintf(&b, "    unschedulable: %s\n", u)
	}

	if r.Density.Defined {
		fmt.Fprintf(&b, "  Break-even density: you pack at u = %.1f%%, the two bills tie at u* = %.1f%% → %s\n",
			r.Density.Achieved*100, r.Density.BreakEven*100, densityAdvice(r.Density))
		fmt.Fprintf(&b, "    of every dollar of node capacity bought: %.1f%% requested by workload pods, "+
			"%.1f%% reserved for kubelet/system, %.1f%% left over (DaemonSet copies + fragmentation)\n",
			r.Density.Achieved*100, r.Density.SystemReservedFraction*100, r.Density.UnusedAllocatableFraction*100)
	}
	for _, blk := range r.Blocks {
		fmt.Fprintf(&b, "  BLOCKED [%s] %s\n", blk.Kind, blk.Reason)
		for _, p := range blk.Pods {
			fmt.Fprintf(&b, "      %s\n", p)
		}
	}
	for _, a := range r.Assumptions {
		fmt.Fprintf(&b, "  assumes: %s\n", a)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	return b.String()
}

// densityAdvice turns the two densities into the lever a human pulls.
func densityAdvice(d Density) string {
	switch {
	case d.BreakEven <= 0:
		return "no break-even: one side has no price"
	case d.Achieved > d.BreakEven:
		return fmt.Sprintf("you are above it, so the node set wins; it would take dropping to %.1f%% density to flip", d.BreakEven*100)
	default:
		return fmt.Sprintf("you are below it, so Fargate wins; packing %.2f× denser would flip it",
			d.BreakEven/nonZero(d.Achieved))
	}
}

func nonZero(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return v
}

func eligibleNote(ok bool) string {
	if ok {
		return ""
	}
	return "  [not eligible — see BLOCKED below; this figure is arithmetic, not advice]"
}

func feasibleNote(ok bool) string {
	if ok {
		return ""
	}
	return "  [infeasible — some pods fit no candidate instance type]"
}

func spotNote(spot bool) string {
	if spot {
		return " (spot)"
	}
	return ""
}

// Insight projects the report into the detection layer for /insights. Severity
// is always informational: a crossover finding is advice about placement, and
// placement migration between domains never auto-applies (§3.2).
func (r Report) Insight() model.Insight {
	return model.Insight{
		Kind:     InsightKind,
		Severity: "info",
		Message:  r.Headline(),
		At:       r.At,
	}
}
