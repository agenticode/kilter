package explain

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Everything in this file is template text over numbers computed elsewhere.
// It exists because §5.9 requires the deterministic planes to produce a
// readable answer with no model configured at all, and because the narrating
// model in §5 is only allowed to quote terms this package computed. Any
// sentence here must be reproducible from the payload it accompanies; none
// of them may introduce a quantity that is not already a field.

// formatUSD renders dollars with 6 decimals — µUSD, the package's exact
// unit — and normalises negative zero, which would otherwise make two
// byte-identical decompositions render differently.
func formatUSD(v float64) string {
	if v == 0 {
		v = 0
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "n/a"
	}
	return "$" + strconv.FormatFloat(v, 'f', 6, 64)
}

// signedUSD renders an hourly amount with an explicit sign, the form a
// decomposition needs: "+$0.420000/h" reads as a contribution, "$0.42/h"
// reads as a level.
func signedUSD(m Micro) string {
	v := m.USD()
	sign := "+"
	if m < 0 {
		sign = "-"
		v = -v
	}
	return sign + "$" + strconv.FormatFloat(v, 'f', 6, 64) + "/h"
}

// signedMonthly renders the same amount projected to a billing month, which
// is the number operators actually argue about.
func signedMonthly(m Micro) string {
	v := m.MonthlyUSD()
	sign := "+"
	if m < 0 {
		sign = "-"
		v = -v
	}
	return sign + "$" + strconv.FormatFloat(v, 'f', 2, 64) + "/mo"
}

func formatRatio(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(v*100, 'f', 1, 64) + "%"
}

func plural(n int64, one, many string) string {
	if n == 1 || n == -1 {
		return one
	}
	return many
}

func nodeCountLabel(deltaNodes int64, pbarMicro float64) string {
	switch {
	case deltaNodes > 0:
		return fmt.Sprintf("%d more %s at the window's starting average price of %s/node-hour",
			deltaNodes, plural(deltaNodes, "node", "nodes"), formatUSD(pbarMicro/MicroPerUSD))
	case deltaNodes < 0:
		return fmt.Sprintf("%d fewer %s at the window's starting average price of %s/node-hour",
			-deltaNodes, plural(deltaNodes, "node", "nodes"), formatUSD(pbarMicro/MicroPerUSD))
	default:
		return "node count unchanged"
	}
}

func spotLabel(m Micro) string {
	switch {
	case m < 0:
		return "a larger share of the fleet runs on spot capacity"
	case m > 0:
		return "a smaller share of the fleet runs on spot capacity"
	default:
		return "the spot/on-demand split did not move the bill"
	}
}

func mixLabel(m Micro) string {
	switch {
	case m < 0:
		return "the fleet shifted toward cheaper instance types"
	case m > 0:
		return "the fleet shifted toward more expensive instance types"
	default:
		return "the instance-type mix did not move the bill"
	}
}

func catalogLabel(m Micro, groups int) string {
	if groups == 0 {
		return "no catalog price changed for any group still running"
	}
	dir := "rose"
	if m < 0 {
		dir = "fell"
	}
	if m == 0 {
		dir = "moved, netting out"
	}
	return fmt.Sprintf("catalog prices %s for %d node %s", dir, groups, plural(int64(groups), "group", "groups"))
}

func kilterActionLabel(nodes int64, plans int) string {
	switch {
	case nodes < 0:
		return fmt.Sprintf("Kilter removed %d %s across %d applied %s",
			-nodes, plural(nodes, "node", "nodes"), plans, plural(int64(plans), "plan", "plans"))
	case nodes > 0:
		return fmt.Sprintf("Kilter added %d %s across %d applied %s",
			nodes, plural(nodes, "node", "nodes"), plans, plural(int64(plans), "plan", "plans"))
	default:
		return fmt.Sprintf("%d applied Kilter %s changed no node count",
			plans, plural(int64(plans), "plan", "plans"))
	}
}

func workloadSetLabel(m Micro, from, to []NamespaceDemand) string {
	a, b := demandByNamespace(from), demandByNamespace(to)
	var added, removed int
	for ns := range b {
		if _, in := a[ns]; !in {
			added++
		}
	}
	for ns := range a {
		if _, in := b[ns]; !in {
			removed++
		}
	}
	switch {
	case added > 0 && removed > 0:
		return fmt.Sprintf("%d new and %d departed %s", added, removed, plural(int64(added+removed), "namespace", "namespaces"))
	case added > 0:
		return fmt.Sprintf("%d new %s", added, plural(int64(added), "namespace", "namespaces"))
	case removed > 0:
		return fmt.Sprintf("%d departed %s", removed, plural(int64(removed), "namespace", "namespaces"))
	default:
		return "the set of namespaces did not change"
	}
}

func workloadScalingLabel(m Micro) string {
	switch {
	case m > 0:
		return "existing namespaces requested more capacity"
	case m < 0:
		return "existing namespaces requested less capacity"
	default:
		return "existing namespaces requested the same capacity"
	}
}

func unattributedLabel(m Micro) string {
	if m == 0 {
		return "every node added or removed is accounted for"
	}
	return "node-count change with no supplied driver behind it"
}

func residualLabel(m Micro) string {
	if m == 0 {
		return "nothing unexplained"
	}
	return "unexplained: cost the supplied evidence does not account for"
}

// Prose renders the attribution as deterministic text with inline citations.
// It is the template half of §5.9's "JSON + template prose": no model is
// involved, and every number in it is a field of the payload.
func (a *Attribution) Prose() string {
	var b strings.Builder
	dir := "rose"
	if a.DeltaMicro < 0 {
		dir = "fell"
	} else if a.DeltaMicro == 0 {
		dir = "did not move"
	}
	fmt.Fprintf(&b, "Between %s and %s the hourly cost of cluster %q %s by %s (%s), from %s to %s.\n",
		a.From.Format("2006-01-02 15:04Z"), a.To.Format("2006-01-02 15:04Z"), a.Cluster, dir,
		signedUSD(a.DeltaMicro), signedMonthly(a.DeltaMicro),
		formatUSD(a.FromUSDPerHour), formatUSD(a.ToUSDPerHour))
	fmt.Fprintf(&b, "Attribution order: %s (see package doc).\n", strings.Join(a.Order, " → "))
	// The charges dimension has its own order and its own argument for it; an
	// answer that states one convention and hides the other is half an audit
	// record. Printed only when charges were actually decomposed.
	if len(a.ChargeOrder) > 0 {
		fmt.Fprintf(&b, "Charge attribution order: %s (see charges.go).\n", strings.Join(a.ChargeOrder, " → "))
	}

	ordered := append([]Term(nil), a.Terms...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ai, aj := abs64(int64(ordered[i].Micro)), abs64(int64(ordered[j].Micro))
		if ai != aj {
			return ai > aj
		}
		return chainIndex(ordered[i].Kind) < chainIndex(ordered[j].Kind)
	})
	for _, t := range ordered {
		writeTerm(&b, t, "  ")
		for _, s := range t.Of {
			writeTerm(&b, s, "      of which ")
		}
	}
	writeTerm(&b, a.Residual, "  ")
	for _, n := range a.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	return b.String()
}

func writeTerm(b *strings.Builder, t Term, indent string) {
	fmt.Fprintf(b, "%s%-16s %14s %14s  %s [%s]\n", indent, t.Kind,
		signedUSD(t.Micro), signedMonthly(t.Micro), t.Label, citeList(t.Evidence))
}

func citeList(ids []ID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, string(id))
	}
	return strings.Join(parts, " ")
}

func chainIndex(kind string) int {
	for i, k := range chainOrder {
		if k == kind {
			return i
		}
	}
	return len(chainOrder)
}
