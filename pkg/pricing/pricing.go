// Package pricing resolves what nodes cost. Resolution order per node:
//
//  1. Explicit cost (kilter.dev/hourly-cost annotation → NodeSpec.HourlyCost)
//  2. Instance-type lookup in the catalog (embedded baseline or custom file)
//  3. Fallback unit economics ($/vCPU-h + $/GiB-h)
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
	HoursPerMonth        = 730
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
}

// Resources returns the schedulable shape of the instance.
func (it InstanceType) Resources() model.Resources {
	return model.Resources{MilliCPU: it.MilliCPU, MemoryBytes: it.MemoryBytes}
}

// Price returns the hourly price for the given lifecycle.
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

// Catalog is an indexed set of instance types.
type Catalog struct {
	instances []InstanceType
	index     map[string]InstanceType // provider + "/" + name
}

// Load parses a catalog from JSON.
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
	c := &Catalog{index: map[string]InstanceType{}}
	for _, it := range f.Instances {
		if it.Name == "" || it.Provider == "" || it.MilliCPU <= 0 || it.MemoryBytes <= 0 || it.HourlyUSD <= 0 {
			return nil, fmt.Errorf("pricing: invalid instance entry %q/%q", it.Provider, it.Name)
		}
		if it.Arch == "" {
			it.Arch = "amd64"
		}
		c.instances = append(c.instances, it)
		c.index[it.Provider+"/"+it.Name] = it
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
	sort.Slice(out, func(i, j int) bool { return out[i].HourlyUSD < out[j].HourlyUSD })
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
)

// NodeHourlyCost resolves one node's hourly price.
func (c *Catalog) NodeHourlyCost(n *model.NodeSpec) (float64, CostSource) {
	if n.HourlyCost > 0 {
		return n.HourlyCost, SourceAnnotation
	}
	if n.InstanceType != "" {
		if it, ok := c.Lookup(n.Provider, n.InstanceType); ok {
			return it.Price(n.Spot), SourceCatalog
		}
	}
	cpu := float64(n.Capacity.MilliCPU) / 1000 * FallbackCPUHourlyUSD
	mem := float64(n.Capacity.MemoryBytes) / (1 << 30) * FallbackGiBHourlyUSD
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

// ClusterCost aggregates a snapshot's node prices.
type ClusterCost struct {
	HourlyUSD  float64    `json:"hourlyUSD"`
	MonthlyUSD float64    `json:"monthlyUSD"`
	Nodes      []NodeCost `json:"nodes"`
}

// SnapshotCost prices every node in the snapshot.
func (c *Catalog) SnapshotCost(snap *model.ClusterSnapshot) ClusterCost {
	out := ClusterCost{}
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		h, src := c.NodeHourlyCost(n)
		out.Nodes = append(out.Nodes, NodeCost{
			Node: n.Name, HourlyUSD: h, MonthlyUSD: h * HoursPerMonth, Source: src, Spot: n.Spot,
		})
		out.HourlyUSD += h
	}
	out.MonthlyUSD = out.HourlyUSD * HoursPerMonth
	return out
}
