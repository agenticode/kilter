// Package awssync generates a live pricing catalog from AWS APIs:
// on-demand prices from the Pricing API (GetProducts) and current spot
// prices from EC2 DescribeSpotPriceHistory. It lives in its own package so
// the AWS SDK never enters Kilter's decision path — the output is a plain
// catalog JSON that `--catalog` loads anywhere, including air-gapped copies.
package awssync

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"

	kpricing "github.com/agenticode/kilter/pkg/pricing"
)

type pricingAPI interface {
	GetProducts(ctx context.Context, in *pricing.GetProductsInput,
		opts ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

type ec2API interface {
	DescribeSpotPriceHistory(ctx context.Context, in *ec2.DescribeSpotPriceHistoryInput,
		opts ...func(*ec2.Options)) (*ec2.DescribeSpotPriceHistoryOutput, error)
}

// Syncer pulls AWS prices for one region.
type Syncer struct {
	Region   string
	Families []string // optional filter, e.g. ["m5","c6i"]; empty = all
	pricing  pricingAPI
	ec2      ec2API
}

// New builds a Syncer with real AWS clients (credentials from environment).
// The Pricing API only lives in us-east-1/eu-central-1/ap-south-1; us-east-1
// is used regardless of the target region.
func New(ctx context.Context, region string, families []string) (*Syncer, error) {
	if region == "" {
		return nil, fmt.Errorf("awssync: region required")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"))
	if err != nil {
		return nil, fmt.Errorf("awssync: load aws config: %w", err)
	}
	ec2cfg := cfg.Copy()
	ec2cfg.Region = region
	return &Syncer{
		Region:   region,
		Families: families,
		pricing:  pricing.NewFromConfig(cfg),
		ec2:      ec2.NewFromConfig(ec2cfg),
	}, nil
}

// newWithClients is the test seam.
func newWithClients(region string, families []string, p pricingAPI, e ec2API) *Syncer {
	return &Syncer{Region: region, Families: families, pricing: p, ec2: e}
}

// Sync fetches prices and renders a catalog JSON document.
func (s *Syncer) Sync(ctx context.Context) ([]byte, error) {
	onDemand, err := s.fetchOnDemand(ctx)
	if err != nil {
		return nil, err
	}
	if len(onDemand) == 0 {
		return nil, fmt.Errorf("awssync: no on-demand prices returned for %s", s.Region)
	}
	spot, err := s.fetchSpot(ctx)
	if err != nil {
		return nil, fmt.Errorf("awssync: spot prices: %w", err)
	}
	for i := range onDemand {
		if sp, ok := spot[onDemand[i].Name]; ok && sp < onDemand[i].HourlyUSD {
			onDemand[i].SpotHourlyUSD = sp
		}
	}
	sort.Slice(onDemand, func(i, j int) bool { return onDemand[i].Name < onDemand[j].Name })
	doc := map[string]any{
		"comment": fmt.Sprintf("AWS %s prices synced %s by `kilter pricing sync-aws`. On-demand: Pricing API; spot: DescribeSpotPriceHistory (latest, averaged across AZs).",
			s.Region, time.Now().UTC().Format(time.RFC3339)),
		"instances": onDemand,
	}
	return json.MarshalIndent(doc, "", "  ")
}

func (s *Syncer) wantFamily(instanceType string) bool {
	if len(s.Families) == 0 {
		return true
	}
	fam := strings.SplitN(instanceType, ".", 2)[0]
	for _, f := range s.Families {
		if fam == f {
			return true
		}
	}
	return false
}

func (s *Syncer) fetchOnDemand(ctx context.Context) ([]kpricing.InstanceType, error) {
	filters := []pricingtypes.Filter{
		{Type: pricingtypes.FilterTypeTermMatch, Field: strPtr("regionCode"), Value: strPtr(s.Region)},
		{Type: pricingtypes.FilterTypeTermMatch, Field: strPtr("operatingSystem"), Value: strPtr("Linux")},
		{Type: pricingtypes.FilterTypeTermMatch, Field: strPtr("tenancy"), Value: strPtr("Shared")},
		{Type: pricingtypes.FilterTypeTermMatch, Field: strPtr("preInstalledSw"), Value: strPtr("NA")},
		{Type: pricingtypes.FilterTypeTermMatch, Field: strPtr("capacitystatus"), Value: strPtr("Used")},
	}
	var out []kpricing.InstanceType
	var next *string
	for {
		resp, err := s.pricing.GetProducts(ctx, &pricing.GetProductsInput{
			ServiceCode: strPtr("AmazonEC2"),
			Filters:     filters,
			NextToken:   next,
		})
		if err != nil {
			return nil, fmt.Errorf("awssync: GetProducts: %w", err)
		}
		for _, raw := range resp.PriceList {
			it, ok := ParsePriceListEntry(raw)
			if ok && s.wantFamily(it.Name) {
				out = append(out, it)
			}
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		next = resp.NextToken
	}
	return out, nil
}

// ParsePriceListEntry converts one Pricing API PriceList JSON document into
// a catalog instance. Returns ok=false for entries that aren't plain
// per-hour Linux instances (metal, unparseable, zero price).
func ParsePriceListEntry(raw string) (kpricing.InstanceType, bool) {
	var doc struct {
		Product struct {
			Attributes map[string]string `json:"attributes"`
		} `json:"product"`
		Terms struct {
			OnDemand map[string]struct {
				PriceDimensions map[string]struct {
					Unit         string            `json:"unit"`
					PricePerUnit map[string]string `json:"pricePerUnit"`
				} `json:"priceDimensions"`
			} `json:"OnDemand"`
		} `json:"terms"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return kpricing.InstanceType{}, false
	}
	attrs := doc.Product.Attributes
	name := attrs["instanceType"]
	if name == "" || strings.Contains(name, "metal") {
		return kpricing.InstanceType{}, false
	}
	vcpu, err := strconv.ParseInt(attrs["vcpu"], 10, 64)
	if err != nil || vcpu <= 0 {
		return kpricing.InstanceType{}, false
	}
	memBytes, ok := parseMemory(attrs["memory"])
	if !ok {
		return kpricing.InstanceType{}, false
	}
	price := 0.0
	for _, term := range doc.Terms.OnDemand {
		for _, dim := range term.PriceDimensions {
			if dim.Unit != "Hrs" {
				continue
			}
			if v, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64); err == nil && v > 0 {
				price = v
			}
		}
	}
	if price <= 0 {
		return kpricing.InstanceType{}, false
	}
	fam := strings.SplitN(name, ".", 2)[0]
	return kpricing.InstanceType{
		Provider:    "aws",
		Name:        name,
		Family:      fam,
		Arch:        archOf(fam),
		MilliCPU:    vcpu * 1000,
		MemoryBytes: memBytes,
		HourlyUSD:   price,
		Burstable:   strings.HasPrefix(fam, "t"),
	}, true
}

var famRe = regexp.MustCompile(`^([a-z]+)([0-9]+)([a-z-]*)$`)

// archOf infers CPU architecture from the family's modifier letters:
// a Graviton family carries "g" after the generation digit (m7g, c6gd, t4g).
func archOf(family string) string {
	m := famRe.FindStringSubmatch(family)
	if m != nil && strings.Contains(m[3], "g") {
		return "arm64"
	}
	return "amd64"
}

var memRe = regexp.MustCompile(`^([0-9.]+)\s*(GiB|MiB|TiB)$`)

func parseMemory(s string) (int64, bool) {
	m := memRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	switch m[2] {
	case "MiB":
		return int64(v * (1 << 20)), true
	case "GiB":
		return int64(v * (1 << 30)), true
	case "TiB":
		return int64(v * (1 << 40)), true
	}
	return 0, false
}

// fetchSpot returns the latest spot price per instance type, averaged across
// availability zones.
func (s *Syncer) fetchSpot(ctx context.Context) (map[string]float64, error) {
	start := time.Now().Add(-2 * time.Hour)
	sums := map[string][]float64{}
	var next *string
	for {
		resp, err := s.ec2.DescribeSpotPriceHistory(ctx, &ec2.DescribeSpotPriceHistoryInput{
			ProductDescriptions: []string{"Linux/UNIX"},
			StartTime:           &start,
			NextToken:           next,
		})
		if err != nil {
			return nil, err
		}
		latest := map[string]map[string]spotPoint{} // type → az → newest
		for _, h := range resp.SpotPriceHistory {
			t := string(h.InstanceType)
			if t == "" || h.SpotPrice == nil || h.Timestamp == nil {
				continue
			}
			price, err := strconv.ParseFloat(*h.SpotPrice, 64)
			if err != nil || price <= 0 {
				continue
			}
			az := str(h.AvailabilityZone)
			if latest[t] == nil {
				latest[t] = map[string]spotPoint{}
			}
			if cur, ok := latest[t][az]; !ok || h.Timestamp.After(cur.at) {
				latest[t][az] = spotPoint{price: price, at: *h.Timestamp}
			}
		}
		for t, azs := range latest {
			for _, pt := range azs {
				sums[t] = append(sums[t], pt.price)
			}
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		next = resp.NextToken
	}
	out := map[string]float64{}
	for t, prices := range sums {
		sum := 0.0
		for _, p := range prices {
			sum += p
		}
		out[t] = sum / float64(len(prices))
	}
	return out, nil
}

type spotPoint struct {
	price float64
	at    time.Time
}

func strPtr(s string) *string { return &s }

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var _ = ec2types.InstanceType("") // keep types import for fakes in tests
