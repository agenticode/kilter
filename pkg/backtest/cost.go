package backtest

import (
	"fmt"
	"math"
	"sort"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/pricing"
)

const bytesPerGiB = float64(1 << 30)

// CostModel prices what a sizing decision reserves and what a safety
// violation costs. Regret is measured in dollars, so both halves need an
// explicit price; hiding either one behind a hard-coded constant would make
// the "conservative vs aggressive" curve unreadable.
//
// Resources are priced per *request*, not per usage: a Kubernetes cluster is
// provisioned from requests, so a request is the thing the operator actually
// pays for. The two rates are deliberately independent of the node catalog by
// default (see DefaultCostModel) and can be derived from a real fleet with
// CostModelFromCatalog.
type CostModel struct {
	// CPUUSDPerCoreHour prices one reserved core for one hour.
	CPUUSDPerCoreHour float64 `json:"cpuUSDPerCoreHour"`
	// MemUSDPerGiBHour prices one reserved GiB for one hour.
	MemUSDPerGiBHour float64 `json:"memUSDPerGiBHour"`
	// IncidentUSD is the operator's price for one violated container-window
	// — a would-OOM or a would-throttle. It is the single most consequential
	// knob in RegretUSD: it sets the exchange rate between "wasted money" and
	// "broke the service", which is exactly the trade-off a rightsizing
	// policy makes. It is reported inside every Scorecard so no comparison
	// can silently assume a different one.
	IncidentUSD float64 `json:"incidentUSD"`
}

// DefaultCostModel derives its resource rates from the cheapest general
// purpose shape in the embedded catalog — m5.large: $0.096/h for 2 vCPU and
// 8 GiB — split 50/50 between the two dimensions, giving $0.024 per core-hour
// and $0.006 per GiB-hour. The 50/50 split is the same convention the
// greenfield estimator uses: neither dimension is privileged, because which
// one binds depends entirely on the workload mix.
//
// IncidentUSD defaults to $50: at the resource rates above, that prices one
// avoidable OOM against roughly ninety container-days of one wasted core,
// i.e. "do not break the service to save pennies" expressed as a number.
func DefaultCostModel() CostModel {
	return CostModel{CPUUSDPerCoreHour: 0.024, MemUSDPerGiBHour: 0.006, IncidentUSD: 50}
}

// withDefaults fills unset (zero) fields from DefaultCostModel. A zero
// IncidentUSD is indistinguishable from "unset" in a struct literal, so it
// takes the default too — an operator who genuinely wants risk priced at zero
// must say so with a negative-free explicit value via Validate-passing
// config, documented in FINDINGS.md.
func (c CostModel) withDefaults() CostModel {
	d := DefaultCostModel()
	if c.CPUUSDPerCoreHour == 0 {
		c.CPUUSDPerCoreHour = d.CPUUSDPerCoreHour
	}
	if c.MemUSDPerGiBHour == 0 {
		c.MemUSDPerGiBHour = d.MemUSDPerGiBHour
	}
	if c.IncidentUSD == 0 {
		c.IncidentUSD = d.IncidentUSD
	}
	return c
}

// Validate rejects garbage. Comparisons are in positive form so a NaN in any
// field fails rather than silently disabling the term it prices.
func (c CostModel) Validate() error {
	if !(c.CPUUSDPerCoreHour > 0) || math.IsInf(c.CPUUSDPerCoreHour, 0) {
		return fmt.Errorf("backtest: CPUUSDPerCoreHour %v must be finite and > 0", c.CPUUSDPerCoreHour)
	}
	if !(c.MemUSDPerGiBHour > 0) || math.IsInf(c.MemUSDPerGiBHour, 0) {
		return fmt.Errorf("backtest: MemUSDPerGiBHour %v must be finite and > 0", c.MemUSDPerGiBHour)
	}
	if !(c.IncidentUSD >= 0) || math.IsInf(c.IncidentUSD, 0) {
		return fmt.Errorf("backtest: IncidentUSD %v must be finite and >= 0", c.IncidentUSD)
	}
	return nil
}

// HourlyUSD prices a reservation. Negative dimensions (which Resources.Sub
// can legitimately produce) clamp to zero rather than minting negative cost.
func (c CostModel) HourlyUSD(r model.Resources) float64 {
	cpu, mem := r.MilliCPU, r.MemoryBytes
	if cpu < 0 {
		cpu = 0
	}
	if mem < 0 {
		mem = 0
	}
	return float64(cpu)/1000*c.CPUUSDPerCoreHour + float64(mem)/bytesPerGiB*c.MemUSDPerGiBHour
}

// CostModelFromCatalog derives resource rates from a real fleet: the priced
// node cost of the snapshot divided by its allocatable capacity, split 50/50
// across the two dimensions. Fargate "nodes" are excluded — they are billed
// per pod, so their reported capacity is not a rate basis.
//
// IncidentUSD is not derivable from a price list and is carried through from
// base (defaulted when unset). A fleet that prices to nothing (no nodes, no
// resolvable prices, zero capacity) returns an error rather than a fabricated
// rate; callers fall back to DefaultCostModel.
func CostModelFromCatalog(cat *pricing.Catalog, snap *model.ClusterSnapshot, base CostModel) (CostModel, error) {
	if cat == nil || snap == nil {
		return base.withDefaults(), fmt.Errorf("backtest: nil catalog or snapshot")
	}
	cost := cat.SnapshotCost(snap)
	// Sort the per-node prices before summing: float addition is not
	// associative, so an unsorted sum over a map- or slice-order-dependent
	// sequence is a determinism bug waiting to happen.
	prices := make([]float64, 0, len(cost.Nodes))
	for _, nc := range cost.Nodes {
		prices = append(prices, nc.HourlyUSD)
	}
	total := sumSorted(prices)

	fargate := map[string]bool{}
	for i := range snap.Nodes {
		if snap.Nodes[i].IsFargate() && snap.Nodes[i].Name != "" {
			fargate[snap.Nodes[i].Name] = true
		}
	}
	cores := make([]float64, 0, len(snap.Nodes))
	gibs := make([]float64, 0, len(snap.Nodes))
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		if n.IsFargate() || fargate[n.Name] {
			continue
		}
		alloc := n.Allocatable
		if alloc.MilliCPU <= 0 || alloc.MemoryBytes <= 0 {
			alloc = n.Capacity
		}
		if alloc.MilliCPU <= 0 || alloc.MemoryBytes <= 0 {
			continue
		}
		cores = append(cores, float64(alloc.MilliCPU)/1000)
		gibs = append(gibs, float64(alloc.MemoryBytes)/bytesPerGiB)
	}
	totalCores, totalGiB := sumSorted(cores), sumSorted(gibs)
	if !(total > 0) || !(totalCores > 0) || !(totalGiB > 0) {
		return base.withDefaults(), fmt.Errorf("backtest: snapshot %q has no priced, sized nodes to derive rates from", snap.ClusterID)
	}
	out := base.withDefaults()
	out.CPUUSDPerCoreHour = 0.5 * total / totalCores
	out.MemUSDPerGiBHour = 0.5 * total / totalGiB
	return out, out.Validate()
}

// sumSorted sums a multiset of float64 in a canonical order. Float addition
// is not associative, so summing the same values in a different sequence can
// produce a different last bit — which is exactly how a "same input, same
// output" guarantee dies (pkg/ecs shipped that bug). Sorting first makes the
// result a function of the multiset alone, independent of how the caller
// happened to enumerate it.
func sumSorted(v []float64) float64 {
	s := append(make([]float64, 0, len(v)), v...)
	sort.Float64s(s)
	total := 0.0
	for _, x := range s {
		total += x
	}
	return total
}

// meanSorted is sumSorted / n, with an empty input reported as zero rather
// than NaN (a scorecard field must always be a number).
func meanSorted(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return sumSorted(v) / float64(len(v))
}

// round6 quantizes a reported float to six decimals. Every Scorecard number
// is money or a ratio; six decimals is a tenth of a cent, well below any
// meaningful difference, and it keeps golden files free of accumulated
// rounding lint like 1.0000000000000002.
func round6(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*1e6) / 1e6
}
