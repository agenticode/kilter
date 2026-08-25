// Package pricing resolves what nodes cost. Resolution order per node:
//
//  1. Explicit cost (kilter.dev/hourly-cost annotation → NodeSpec.HourlyCost)
//  2. Instance-type lookup in the catalog (embedded baseline or custom file)
//  3. Fallback unit economics ($/vCPU-h + $/GiB-h)
//
// Pods with no node to be priced by — EKS Fargate pods, each in its own
// single-pod VM — are priced instead by their billed vCPU/memory tier; see
// fargate.go.
//
// Embedded prices are a baseline (us-east-1 class), good for relative savings
// math; exact billing belongs to your cloud invoice. Everything is overridable.
package pricing

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/agenticode/kilter/pkg/model"
)

//go:embed catalog.json
var embeddedCatalog []byte

// Fallback unit prices, derived from general-purpose cloud instance economics.
const (
	FallbackCPUHourlyUSD = 0.0330 // per vCPU-hour
	FallbackGiBHourlyUSD = 0.0044 // per GiB-hour
	// HoursPerMonth converts hourly to monthly cost using the billing-average
	// month (8760 h/year ÷ 12), the same convention cloud calculators use.
	HoursPerMonth = 730
)

// InstanceType describes one purchasable node shape.
type InstanceType struct {
	Provider      string  `json:"provider"`
	Name          string  `json:"name"`
	Family        string  `json:"family"`
	Arch          string  `json:"arch"` // amd64 | arm64
	MilliCPU      int64   `json:"milliCPU"`
	MemoryBytes   int64   `json:"memoryBytes"`
	HourlyUSD     float64 `json:"hourlyUSD"`
	SpotHourlyUSD float64 `json:"spotHourlyUSD,omitempty"`
	// Burstable marks shared/credit-based CPU (AWS t-family, GCP e2-micro/
	// small/medium, Azure B-series). Planners exclude these for sustained
	// workloads by default: their price assumes you do NOT use the vCPUs
	// continuously.
	Burstable bool `json:"burstable,omitempty"`
}

// Resources returns the schedulable shape of the instance.
func (it InstanceType) Resources() model.Resources {
	return model.Resources{MilliCPU: it.MilliCPU, MemoryBytes: it.MemoryBytes}
}

// Price returns the hourly price for the given lifecycle. Asking for spot
// when no positive spot price is known falls back to the on-demand price —
// overstating cost rather than inventing a discount.
func (it InstanceType) Price(spot bool) float64 {
	if spot && it.SpotHourlyUSD > 0 {
		return it.SpotHourlyUSD
	}
	return it.HourlyUSD
}

type catalogFile struct {
	Comment   string         `json:"comment,omitempty"`
	Instances []InstanceType `json:"instances"`
}

// Catalog is an indexed set of instance types, plus the Fargate rate table
// used to price pods that have no node to be priced by (see fargate.go).
type Catalog struct {
	instances []InstanceType
	index     map[string]InstanceType // provider + "/" + name
	fargate   FargateRates
}

// Load parses a catalog from JSON and validates every entry: positive shape
// and on-demand price, non-negative spot price, a known architecture, and no
// duplicate provider/name pairs. Catalog numbers feed decisions directly, so
// bad data fails loudly here instead of silently skewing plans later.
func Load(r io.Reader) (*Catalog, error) {
	var f catalogFile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("pricing: parse catalog: %w", err)
	}
	if len(f.Instances) == 0 {
		return nil, fmt.Errorf("pricing: catalog has no instances")
	}
	c := &Catalog{index: map[string]InstanceType{}, fargate: DefaultFargateRates()}
	for _, it := range f.Instances {
		if it.Name == "" || it.Provider == "" || it.MilliCPU <= 0 || it.MemoryBytes <= 0 || it.HourlyUSD <= 0 {
			return nil, fmt.Errorf("pricing: invalid instance entry %q/%q", it.Provider, it.Name)
		}
		if it.SpotHourlyUSD < 0 {
			return nil, fmt.Errorf("pricing: negative spot price for %q/%q", it.Provider, it.Name)
		}
		if it.Arch == "" {
			it.Arch = "amd64"
		}
		if it.Arch != "amd64" && it.Arch != "arm64" {
			return nil, fmt.Errorf("pricing: unknown arch %q for %q/%q (want amd64 or arm64)", it.Arch, it.Provider, it.Name)
		}
		key := it.Provider + "/" + it.Name
		if _, dup := c.index[key]; dup {
			// A duplicate would double-count in Candidates while Lookup sees
			// only one of the two prices — reject rather than pick a winner.
			return nil, fmt.Errorf("pricing: duplicate instance entry %q", key)
		}
		c.instances = append(c.instances, it)
		c.index[key] = it
	}
	return c, nil
}

// LoadFile loads a custom catalog from disk.
func LoadFile(path string) (*Catalog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Load(f)
}

// Embedded returns the built-in baseline catalog.
func Embedded() *Catalog {
	c, err := Load(bytes.NewReader(embeddedCatalog))
	if err != nil {
		panic("pricing: embedded catalog corrupt: " + err.Error())
	}
	return c
}

// Lookup finds an instance type by provider and name.
func (c *Catalog) Lookup(provider, name string) (InstanceType, bool) {
	it, ok := c.index[provider+"/"+name]
	return it, ok
}

// Candidates returns instance types for a provider (all providers if empty),
// optionally filtered by architecture, sorted by hourly price ascending.
// Price ties break by (provider, name) so the ordering — and therefore any
// plan derived from it — is reproducible regardless of catalog file order.
func (c *Catalog) Candidates(provider, arch string) []InstanceType {
	var out []InstanceType
	for _, it := range c.instances {
		if provider != "" && it.Provider != provider {
			continue
		}
		if arch != "" && it.Arch != arch {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HourlyUSD != b.HourlyUSD {
			return a.HourlyUSD < b.HourlyUSD
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Name < b.Name
	})
	return out
}

// Len returns the number of instance types in the catalog.
func (c *Catalog) Len() int { return len(c.instances) }

// CostSource explains where a node's price came from.
type CostSource string

const (
	SourceAnnotation CostSource = "annotation"
	SourceCatalog    CostSource = "catalog"
	SourceFallback   CostSource = "fallback"
	// SourceFargateNode marks a node that has no node price at all: it is a
	// Fargate single-pod VM, billed as a pod configuration instead.
	SourceFargateNode CostSource = "fargate-node"
)

// NodeHourlyCost resolves one node's hourly price via annotation → catalog →
// fallback. The result is always finite and ≥ 0 for a Load-validated catalog:
// a non-finite annotation is ignored (collectors guard this too, but snapshots
// can be built by other paths) and garbage negative capacity clamps to zero
// instead of pricing a node below free.
func (c *Catalog) NodeHourlyCost(n *model.NodeSpec) (float64, CostSource) {
	// A Fargate "node" is a single-pod VM. Its reported capacity is not what
	// AWS bills — the pod's quantized configuration is (§4.1) — and that
	// capacity is routinely larger than the billed one, so pricing it here at
	// any source, annotation included, is a silent overcharge. It has no node
	// price: SnapshotCost prices its pod instead.
	if n.IsFargate() {
		return 0, SourceFargateNode
	}
	if n.HourlyCost > 0 && !math.IsInf(n.HourlyCost, 1) {
		return n.HourlyCost, SourceAnnotation
	}
	if n.InstanceType != "" {
		if it, ok := c.Lookup(n.Provider, n.InstanceType); ok {
			return it.Price(n.Spot), SourceCatalog
		}
	}
	cpu := float64(max(n.Capacity.MilliCPU, 0)) / 1000 * FallbackCPUHourlyUSD
	mem := float64(max(n.Capacity.MemoryBytes, 0)) / (1 << 30) * FallbackGiBHourlyUSD
	cost := cpu + mem
	if n.Spot {
		cost *= 0.35 // typical spot discount
	}
	return cost, SourceFallback
}

// NodeCost is one node's resolved price.
type NodeCost struct {
	Node       string     `json:"node"`
	HourlyUSD  float64    `json:"hourlyUSD"`
	MonthlyUSD float64    `json:"monthlyUSD"`
	Source     CostSource `json:"source"`
	Spot       bool       `json:"spot,omitempty"`
}

// ClusterCost aggregates a snapshot's node prices and its Fargate pod prices.
type ClusterCost struct {
	HourlyUSD  float64    `json:"hourlyUSD"`
	MonthlyUSD float64    `json:"monthlyUSD"`
	Nodes      []NodeCost `json:"nodes"`
	// Fargate holds the per-pod prices of Fargate-hosted pods. Their
	// single-pod VMs never appear in Nodes.
	Fargate          []FargatePodCost `json:"fargate,omitempty"`
	FargateHourlyUSD float64          `json:"fargateHourlyUSD,omitempty"`
	// Warnings records billing inputs that did not add up (a pod too large for
	// Fargate, an unparseable CapacityProvisioned, a quantizer/AWS
	// disagreement). Nothing here is silently absorbed.
	Warnings []string `json:"warnings,omitempty"`
}

// SnapshotCost prices a snapshot: node-backed nodes by their shape, Fargate
// pods by their billed configuration. A nil snapshot prices as an empty
// cluster.
//
// Fargate is not node math. A Fargate "node" is a single-pod VM whose reported
// capacity is not the bill, so it is excluded from Nodes entirely and its pod
// is priced by ProvisionedCapacity if AWS stamped one, else by the quantizer
// (§4.1.2) — never by node capacity, which is the silent mispricing this
// exists to remove. Pods on Fargate nodes are priced in snapshot order, so the
// result is deterministic.
//
// Invariant: HourlyUSD equals the sum of the per-node entries plus the sum of
// the per-Fargate-pod entries, and FargateHourlyUSD equals the latter.
func (c *Catalog) SnapshotCost(snap *model.ClusterSnapshot) ClusterCost {
	out := ClusterCost{}
	if snap == nil {
		return out
	}
	fargateNodes := make(map[string]bool)
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		if n.IsFargate() {
			if n.Name != "" { // an unnamed node must not capture unscheduled pods
				fargateNodes[n.Name] = true
			}
			continue
		}
		h, src := c.NodeHourlyCost(n)
		out.Nodes = append(out.Nodes, NodeCost{
			Node: n.Name, HourlyUSD: h, MonthlyUSD: h * HoursPerMonth, Source: src, Spot: n.Spot,
		})
		out.HourlyUSD += h
	}
	if len(fargateNodes) > 0 {
		rates := c.FargateRates()
		for i := range snap.Pods {
			p := &snap.Pods[i]
			if !fargateNodes[p.NodeName] {
				continue
			}
			out.Warnings = append(out.Warnings, FargatePodWarnings(p)...)
			cfg, src, err := FargatePodConfig(p)
			if err != nil {
				// A pod above the Fargate ceiling cannot be scheduled, so it
				// has no bill to report. Loudly unpriced beats a fabricated
				// number in either direction.
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"fargate pod %s/%s left unpriced: %v", p.Namespace, p.Name, err))
				continue
			}
			h := rates.Cost(cfg)
			out.Fargate = append(out.Fargate, FargatePodCost{
				Pod: p.Namespace + "/" + p.Name, UID: p.UID, Node: p.NodeName,
				Config: cfg, HourlyUSD: h, MonthlyUSD: h * HoursPerMonth, Source: src,
			})
			out.FargateHourlyUSD += h
			out.HourlyUSD += h
		}
	}
	out.MonthlyUSD = out.HourlyUSD * HoursPerMonth
	return out
}
