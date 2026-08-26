// Fargate pricing. AWS does not bill a Fargate pod for what it requests: it
// adds ~256 MiB of Kubernetes overhead to the memory request, rounds the pair
// up to the next valid vCPU/memory configuration, and bills that. Pricing a
// Fargate pod by its node's shape (the node is a single-pod VM whose reported
// capacity is unrelated to the bill) or by its raw requests is wrong in the
// expensive direction — AWS's own worked example bills +59 % over the naive
// estimate (§4.1.1 of docs/design/compute-domains.md).
//
// Three invariants hold everywhere in this file:
//
//  1. Quantization is rate-independent. Which tier a pod lands on is an AWS
//     scheduling fact, not an economic one, so overriding the rate table can
//     never move a pod to a different tier — only reprice the one it is on.
//  2. EKS Fargate has exactly one billing platform: Linux, x86_64, on-demand.
//     No Fargate Spot, no Graviton (both are ECS-only). That is enforced by
//     the type system (see Platform), not by a comment.
//  3. Fargate pods are never priced by node capacity. SnapshotCost splits them
//     out before node math runs.

package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/agenticode/kilter/pkg/model"
)

// FargateOverheadBytes is the memory Fargate adds to every pod's request for
// kubelet, kube-proxy and containerd before rounding to a billable tier.
// [verified: fargate-pod-configuration.html]
const FargateOverheadBytes int64 = 256 << 20

// Platform identifies the engine/architecture/market a Fargate pod is billed
// under. EKS Fargate has exactly one, EKSLinuxX86, and this package exposes
// no other value: Fargate Spot and ARM/Graviton exist on ECS only
// ("Amazon EKS doesn't support Fargate Spot"; "Can run workloads that require
// Arm processors: No" — both [verified: fargate.html]).
//
// The single unexported field is the point: a caller cannot construct a spot
// or arm64 Platform, so "move these EKS pods to Fargate Spot/Graviton" — the
// most common unactionable Fargate recommendation — is unrepresentable rather
// than merely discouraged. The zero Platform is invalid.
type Platform struct{ id string }

// EKSLinuxX86 is the only platform EKS Fargate bills under.
var EKSLinuxX86 = Platform{id: "eks/linux/x86_64/on-demand"}

// String returns the platform identifier ("" for the invalid zero value).
func (p Platform) String() string { return p.id }

// Valid reports whether p is a platform this package can price.
func (p Platform) Valid() bool { return p.id == EKSLinuxX86.id }

// Embedded Fargate rates, us-east-1, Linux/x86, on-demand
// [verified: https://aws.amazon.com/fargate/pricing/ — $0.000011244/vCPU-s and
// $0.000001235/GB-s, expressed here per hour]. Like the instance catalog these
// are a baseline for relative math; exact billing belongs to the invoice.
const (
	FargateVCPUHourlyUSD = 0.04048
	FargateGBHourlyUSD   = 0.004445
)

// FargateRates prices one Fargate configuration. It is deliberately a pair of
// x86 on-demand rates and nothing else: there is no spot discount field and no
// ARM rate field, because on EKS neither exists (see Platform).
type FargateRates struct {
	// Platform is always EKSLinuxX86; it is set by the constructors and is not
	// part of the override file's JSON.
	Platform Platform `json:"-"`
	// Region is the region these numbers are quoted in, matching every other
	// rate table in this package (see rates.go). It is a label, not a gate:
	// pricing works without it. An override file cannot set it — the file
	// states rates, not which region they came from — so rates loaded from
	// one carry the empty Region, which is the honest answer.
	Region        Region  `json:"-"`
	VCPUHourlyUSD float64 `json:"vcpuHourlyUSD"`
	GBHourlyUSD   float64 `json:"gbHourlyUSD"`
}

// DefaultFargateRates returns the embedded baseline rates.
func DefaultFargateRates() FargateRates {
	return FargateRates{
		Platform:      EKSLinuxX86,
		Region:        DefaultRegion,
		VCPUHourlyUSD: FargateVCPUHourlyUSD,
		GBHourlyUSD:   FargateGBHourlyUSD,
	}
}

// valid reports whether the rates can price anything.
func (r FargateRates) valid() bool {
	return r.Platform.Valid() &&
		r.VCPUHourlyUSD > 0 && !math.IsInf(r.VCPUHourlyUSD, 1) &&
		r.GBHourlyUSD > 0 && !math.IsInf(r.GBHourlyUSD, 1)
}

// Cost returns the hourly USD price of a configuration:
// P(v, g) = v·rate_vcpu + g·rate_gb (§4.1).
func (r FargateRates) Cost(c FargateConfig) float64 {
	return float64(c.MilliCPU)/1000*r.VCPUHourlyUSD + c.MemoryGB()*r.GBHourlyUSD
}

// MonthlyCost returns Cost scaled by the billing-average month.
func (r FargateRates) MonthlyCost(c FargateConfig) float64 { return r.Cost(c) * HoursPerMonth }

// LoadFargateRates parses a rate override, mirroring how Load parses a catalog:
// unknown fields are rejected, so an override file that tries to introduce
// `spotDiscount`, `armVCPUHourlyUSD` or any other ECS-only dimension fails
// loudly instead of being silently ignored on a platform that cannot use it.
func LoadFargateRates(r io.Reader) (FargateRates, error) {
	out := FargateRates{Platform: EKSLinuxX86}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return FargateRates{}, fmt.Errorf("pricing: parse fargate rates: %w", err)
	}
	out.Platform = EKSLinuxX86
	if !out.valid() {
		return FargateRates{}, fmt.Errorf("pricing: invalid fargate rates (need positive finite vcpuHourlyUSD and gbHourlyUSD, got %v and %v)",
			out.VCPUHourlyUSD, out.GBHourlyUSD)
	}
	return out, nil
}

// LoadFargateRatesFile loads a rate override from disk.
func LoadFargateRatesFile(path string) (FargateRates, error) {
	f, err := os.Open(path)
	if err != nil {
		return FargateRates{}, err
	}
	defer f.Close()
	return LoadFargateRates(f)
}

// FargateConfig is one valid Fargate compute configuration — a billable tier.
// Memory is held in MiB (integer) rather than fractional GB so tier identity
// is exact comparison, never a float epsilon question.
type FargateConfig struct {
	MilliCPU  int64 `json:"milliCPU"`
	MemoryMiB int64 `json:"memoryMiB"`
}

// MemoryBytes returns the configuration's memory in bytes.
func (c FargateConfig) MemoryBytes() int64 { return c.MemoryMiB << 20 }

// MemoryGB returns the configuration's memory in GB as AWS labels it (binary
// GB, i.e. GiB: the "0.5GB" tier is 512 MiB).
func (c FargateConfig) MemoryGB() float64 { return float64(c.MemoryMiB) / 1024 }

// VCPU returns the configuration's vCPU count.
func (c FargateConfig) VCPU() float64 { return float64(c.MilliCPU) / 1000 }

// Resources renders the configuration in scheduler units.
func (c FargateConfig) Resources() model.Resources {
	return model.Resources{MilliCPU: c.MilliCPU, MemoryBytes: c.MemoryBytes()}
}

// IsZero reports whether c is the zero value (no configuration).
func (c FargateConfig) IsZero() bool { return c.MilliCPU == 0 && c.MemoryMiB == 0 }

// String renders the configuration exactly as AWS stamps it on the pod's
// CapacityProvisioned annotation, e.g. "0.25vCPU 0.5GB".
func (c FargateConfig) String() string {
	return strconv.FormatFloat(c.VCPU(), 'g', -1, 64) + "vCPU " +
		strconv.FormatFloat(c.MemoryGB(), 'g', -1, 64) + "GB"
}

// fargateTier is one vCPU row of the §4.1 configuration table.
type fargateTier struct {
	milliCPU int64
	memMiB   []int64
}

// gibRange expands an inclusive GB range with a GB step into MiB values.
func gibRange(minGB, maxGB, stepGB int64) []int64 {
	out := make([]int64, 0, (maxGB-minGB)/stepGB+1)
	for g := minGB; g <= maxGB; g += stepGB {
		out = append(out, g*1024)
	}
	return out
}

// fargateTiers is the complete valid-configuration table
// [verified: fargate-pod-configuration.html], in ascending vCPU order:
//
//	0.25 vCPU | 0.5, 1, 2 GB
//	0.5  vCPU | 1, 2, 3, 4 GB
//	1    vCPU | 2–8 GB, 1-GB steps
//	2    vCPU | 4–16 GB, 1-GB steps
//	4    vCPU | 8–30 GB, 1-GB steps
//	8    vCPU | 16–60 GB, 4-GB steps
//	16   vCPU | 32–120 GB, 8-GB steps
var fargateTiers = []fargateTier{
	{milliCPU: 250, memMiB: []int64{512, 1024, 2048}}, // the only non-arithmetic row
	{milliCPU: 500, memMiB: gibRange(1, 4, 1)},
	{milliCPU: 1000, memMiB: gibRange(2, 8, 1)},
	{milliCPU: 2000, memMiB: gibRange(4, 16, 1)},
	{milliCPU: 4000, memMiB: gibRange(8, 30, 1)},
	{milliCPU: 8000, memMiB: gibRange(16, 60, 4)},
	{milliCPU: 16000, memMiB: gibRange(32, 120, 8)},
}

// fargateConfigs is the flattened table in canonical order: vCPU ascending,
// then memory ascending. Rounding walks it in order, so the first entry that
// fits is both the smallest valid configuration and (for any positive rates)
// the cheapest one — see TestQuantizeRoundUpIsAlsoCheapest.
var fargateConfigs = func() []FargateConfig {
	var out []FargateConfig
	for _, t := range fargateTiers {
		for _, m := range t.memMiB {
			out = append(out, FargateConfig{MilliCPU: t.milliCPU, MemoryMiB: m})
		}
	}
	return out
}()

// FargateMinConfig is the configuration an unspecified request lands on.
var FargateMinConfig = fargateConfigs[0]

// FargateMaxConfig is the Fargate ceiling: 16 vCPU / 120 GB.
var FargateMaxConfig = fargateConfigs[len(fargateConfigs)-1]

// FargateConfigs returns a copy of the valid configuration table in canonical
// order (vCPU ascending, then memory ascending). It is a copy so no caller can
// mutate the table the quantizer reads.
func FargateConfigs() []FargateConfig {
	out := make([]FargateConfig, len(fargateConfigs))
	copy(out, fargateConfigs)
	return out
}

// ErrFargateTooLarge reports a pod that cannot run on Fargate at all: after
// the Kubernetes memory overhead it exceeds the 16 vCPU / 120 GB ceiling.
// Callers must handle it — silently pricing such a pod at the ceiling would
// invent a bill for a pod that will never be scheduled.
var ErrFargateTooLarge = errors.New("pricing: exceeds the Fargate maximum configuration (16 vCPU / 120 GB)")

// clampNonNegative floors both dimensions at zero. Requests arrive from
// snapshots that other paths may have built, and a negative request must not
// be able to shrink the effective demand below "unspecified".
func clampNonNegative(r model.Resources) model.Resources {
	return model.Resources{MilliCPU: max(r.MilliCPU, 0), MemoryBytes: max(r.MemoryBytes, 0)}
}

// FargateEffectiveRequests returns the request Fargate sizes a pod from:
// per dimension, the maximum of the summed long-running container requests and
// the largest init container request. Init containers run to completion one at
// a time, so they never stack with each other or with the app containers —
// they set a floor. [verified: fargate-pod-configuration.html]
func FargateEffectiveRequests(p *model.PodSpec) model.Resources {
	if p == nil {
		return model.Resources{}
	}
	return clampNonNegative(p.Requests()).Max(clampNonNegative(p.InitRequests))
}

// Quantize maps a pod's requests to the configuration AWS will bill it at.
//
// req is the sum over long-running containers, initReq the element-wise
// maximum over init containers (model.PodSpec.InitRequests); the effective
// request is the per-dimension maximum of the two. Fargate then adds
// FargateOverheadBytes of memory and rounds *up* to the closest valid
// configuration — both dimensions must fit, and the memory tiers available
// depend on the vCPU tier, which is what produces the overhead cliff of
// §4.1.1: 1 vCPU / 8 GB requested bills as 2 vCPU / 9 GB.
//
// An unspecified (zero) request lands on the smallest configuration, because
// the overhead alone (256 MiB) still fits 0.25 vCPU / 0.5 GB. Negative
// garbage clamps to zero rather than shrinking demand.
//
// Quantize is pure, deterministic, and independent of the rate table: rates
// price a tier, they never choose one.
func Quantize(req, initReq model.Resources) (FargateConfig, error) {
	eff := clampNonNegative(req).Max(clampNonNegative(initReq))
	// Saturating add: a garbage MaxInt64 request must stay absurdly large and
	// be rejected, never wrap negative into "fits the smallest tier".
	need := eff.Add(model.Resources{MemoryBytes: FargateOverheadBytes})

	for _, c := range fargateConfigs {
		if c.MilliCPU >= need.MilliCPU && c.MemoryBytes() >= need.MemoryBytes {
			return c, nil
		}
	}
	return FargateConfig{}, fmt.Errorf("%w: need %s (%s requested + %d MiB overhead)",
		ErrFargateTooLarge, need, eff, FargateOverheadBytes>>20)
}

// QuantizePod is Quantize applied to a pod's own requests.
func QuantizePod(p *model.PodSpec) (FargateConfig, error) {
	if p == nil {
		return FargateConfig{}, fmt.Errorf("pricing: nil pod")
	}
	return Quantize(p.Requests(), p.InitRequests)
}

// FargateConfigFor returns the valid configuration exactly matching r, if any.
// Used to recognize an AWS-stamped CapacityProvisioned value as a real tier.
func FargateConfigFor(r model.Resources) (FargateConfig, bool) {
	for _, c := range fargateConfigs {
		if c.MilliCPU == r.MilliCPU && c.MemoryBytes() == r.MemoryBytes {
			return c, true
		}
	}
	return FargateConfig{}, false
}

// ParseCapacityProvisioned parses the value of a Fargate pod's
// CapacityProvisioned annotation ("0.25vCPU 0.5GB") into the tier it names.
// The annotation is AWS's statement of what the pod costs, so anything that is
// not a valid configuration is an error the collector should surface, not a
// value to guess at.
func ParseCapacityProvisioned(s string) (FargateConfig, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return FargateConfig{}, fmt.Errorf("pricing: malformed CapacityProvisioned %q (want \"<n>vCPU <n>GB\")", s)
	}
	cpu, err := parseSuffixed(fields[0], "vcpu")
	if err != nil {
		return FargateConfig{}, fmt.Errorf("pricing: malformed CapacityProvisioned %q: %w", s, err)
	}
	mem, err := parseSuffixed(fields[1], "gb")
	if err != nil {
		return FargateConfig{}, fmt.Errorf("pricing: malformed CapacityProvisioned %q: %w", s, err)
	}
	got := model.Resources{
		MilliCPU:    int64(math.Round(cpu * 1000)),
		MemoryBytes: int64(math.Round(mem*1024)) << 20,
	}
	c, ok := FargateConfigFor(got)
	if !ok {
		return FargateConfig{}, fmt.Errorf("pricing: CapacityProvisioned %q is not a valid Fargate configuration", s)
	}
	return c, nil
}

// parseSuffixed parses "0.25vCPU" given the lowercased suffix "vcpu".
func parseSuffixed(field, suffix string) (float64, error) {
	num := strings.TrimSpace(strings.TrimSuffix(strings.ToLower(field), suffix))
	if num == strings.ToLower(field) {
		return 0, fmt.Errorf("field %q lacks the %q suffix", field, suffix)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("field %q: %w", field, err)
	}
	if v <= 0 || math.IsInf(v, 0) {
		return 0, fmt.Errorf("field %q: non-positive or infinite value", field)
	}
	return v, nil
}

// FargatePod is one Fargate-hosted pod, separated from the node-backed cluster.
type FargatePod struct {
	// Pod is a copy of the snapshot's pod (slice and map fields are shared with
	// the snapshot; treat them as read-only, as the rest of the engine does).
	Pod model.PodSpec `json:"pod"`
	// NodeName is the single-pod Fargate VM the pod runs on. It exists only as
	// an identifier — its capacity is never priced or packed.
	NodeName string `json:"nodeName,omitempty"`
}

// SplitFargate partitions a cluster snapshot into the node-backed cluster and
// the Fargate pod set. Fargate "nodes" — one single-pod VM per pod, labelled
// eks.amazonaws.com/compute-type=fargate — and the pods on them are removed
// from the returned snapshot, so bin-packing, consolidation and node pricing
// structurally cannot see them (§7 trap 7: fed to binpack they produce
// nonsense consolidation plans and double-counted savings).
//
// The returned snapshot always has freshly allocated Nodes and Pods slices, so
// a caller may rewrite them without touching the input; Workloads, PDBs and
// Usage are carried through unchanged, because pkg/recommend is reused verbatim
// for Fargate containers and must keep their histories. Pods are returned in
// snapshot order — the split is deterministic. A nil snapshot splits to
// (nil, nil).
func SplitFargate(s *model.ClusterSnapshot) (*model.ClusterSnapshot, []FargatePod) {
	if s == nil {
		return nil, nil
	}
	fargateNodes := make(map[string]bool)
	for i := range s.Nodes {
		if s.Nodes[i].IsFargate() {
			fargateNodes[s.Nodes[i].Name] = true
		}
	}

	out := *s
	out.Nodes = make([]model.NodeSpec, 0, len(s.Nodes))
	for i := range s.Nodes {
		if fargateNodes[s.Nodes[i].Name] {
			continue
		}
		out.Nodes = append(out.Nodes, s.Nodes[i])
	}
	var fargate []FargatePod
	out.Pods = make([]model.PodSpec, 0, len(s.Pods))
	for i := range s.Pods {
		if fargateNodes[s.Pods[i].NodeName] {
			fargate = append(fargate, FargatePod{Pod: s.Pods[i], NodeName: s.Pods[i].NodeName})
			continue
		}
		out.Pods = append(out.Pods, s.Pods[i])
	}
	return &out, fargate
}

// Fargate cost sources, extending the node-pricing sources.
const (
	// SourceProvisioned: priced from the AWS-stamped CapacityProvisioned
	// annotation — ground truth for the bill.
	SourceProvisioned CostSource = "provisioned"
	// SourceQuantized: priced from requests run through Quantize.
	SourceQuantized CostSource = "quantized"
)

// FargatePodConfig resolves the configuration a Fargate pod is billed at,
// with the §4.1.2 precedence:
//
//  1. ProvisionedCapacity (the CapacityProvisioned annotation) when it names a
//     valid configuration — AWS's own statement of what the pod costs;
//  2. otherwise Quantize over the pod's effective requests;
//  3. never the node's capacity, instance type, or hourly-cost annotation. A
//     Fargate node is a single-pod VM whose reported shape is not the bill.
//
// A ProvisionedCapacity that is not a valid configuration is not trusted: it
// falls through to the quantizer, and SnapshotCost reports it as a warning.
func FargatePodConfig(p *model.PodSpec) (FargateConfig, CostSource, error) {
	if p == nil {
		return FargateConfig{}, "", fmt.Errorf("pricing: nil pod")
	}
	if !p.ProvisionedCapacity.IsZero() {
		if c, ok := FargateConfigFor(p.ProvisionedCapacity); ok {
			return c, SourceProvisioned, nil
		}
	}
	c, err := Quantize(p.Requests(), p.InitRequests)
	if err != nil {
		return FargateConfig{}, SourceQuantized, err
	}
	return c, SourceQuantized, nil
}

// FargatePodCost is one Fargate pod's resolved price.
type FargatePodCost struct {
	Pod        string        `json:"pod"` // namespace/name
	UID        string        `json:"uid,omitempty"`
	Node       string        `json:"node,omitempty"`
	Config     FargateConfig `json:"config"`
	HourlyUSD  float64       `json:"hourlyUSD"`
	MonthlyUSD float64       `json:"monthlyUSD"`
	Source     CostSource    `json:"source"`
}

// FargateRates returns the rate table this catalog prices Fargate with,
// falling back to the embedded baseline when none was set (a Catalog built
// before rates existed must not price Fargate at zero).
func (c *Catalog) FargateRates() FargateRates {
	if c == nil || !c.fargate.valid() {
		return DefaultFargateRates()
	}
	return c.fargate
}

// WithFargateRates returns a copy of the catalog pricing Fargate at r. Invalid
// rates are rejected rather than silently ignored.
func (c *Catalog) WithFargateRates(r FargateRates) (*Catalog, error) {
	if !r.valid() {
		return nil, fmt.Errorf("pricing: invalid fargate rates %+v", r)
	}
	out := *c
	out.fargate = r
	return &out, nil
}

// FargatePodHourlyCost resolves one Fargate pod's hourly price. It mirrors
// NodeHourlyCost: always finite and ≥ 0. A pod too large for Fargate cannot be
// priced at all — it would never be scheduled — so it returns 0 with the
// underlying error rather than inventing a bill.
func (c *Catalog) FargatePodHourlyCost(p *model.PodSpec) (float64, CostSource, error) {
	cfg, src, err := FargatePodConfig(p)
	if err != nil {
		return 0, src, err
	}
	return c.FargateRates().Cost(cfg), src, nil
}

// FargatePodWarnings reports data-integrity problems with a Fargate pod's
// billing inputs, in a fixed order. Nil means the pod prices cleanly.
//
// The second check is the production validation §4.1.2 asks for: the quantizer
// is a model of an AWS behaviour, and every pod carrying a CapacityProvisioned
// annotation is a live test case for it. A disagreement is a bug in this
// package (or an AWS change), and is surfaced rather than silently absorbed.
func FargatePodWarnings(p *model.PodSpec) []string {
	if p == nil {
		return nil
	}
	var out []string
	name := p.Namespace + "/" + p.Name
	provisioned, validProvisioned := FargateConfigFor(p.ProvisionedCapacity)
	if !p.ProvisionedCapacity.IsZero() && !validProvisioned {
		out = append(out, fmt.Sprintf(
			"fargate pod %s: provisionedCapacity %s is not a valid Fargate configuration; priced from requests instead",
			name, p.ProvisionedCapacity))
	}
	if validProvisioned {
		if q, err := Quantize(p.Requests(), p.InitRequests); err == nil && q != provisioned {
			out = append(out, fmt.Sprintf(
				"fargate pod %s: quantizer says %q but AWS provisioned %q — quantizer bug or AWS tier change; billed at the AWS value",
				name, q, provisioned))
		}
	}
	return out
}
