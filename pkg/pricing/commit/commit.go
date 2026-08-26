// Package commit models AWS compute commitments — Reserved Instances,
// Reserved DB Instances and Savings Plans — and prices usage through them.
//
// # Why this package exists
//
// A rightsizing recommendation changes *usage*; a commitment keeps billing
// regardless. Subtracting list prices therefore reports savings the invoice
// never shows, and can hide an outright cost increase: docs/design/
// compute-domains.md §4.4 ex.1 documents a verified +135 % case where the
// optimizer cheerfully reports a 15 % saving. The only honest number is a
// bill delta:
//
//	NetSavings(rec) = Bill(usage) − Bill(apply(rec, usage))
//
// [Inventory.Bill] is a pure re-implementation of the application order AWS
// documents, in this precedence:
//
//  1. zonal Reserved Instances — exact match (AZ, instance type, platform,
//     tenancy);
//  2. regional Reserved Instances — AZ-flexible, and size-flexible within the
//     family via normalization units, applied smallest instance first;
//  3. Reserved DB Instances — RDS only, size-flexible within the instance
//     class type when the engine allows it (see rds.go);
//  4. EC2 Instance Savings Plans — before Compute SPs, being narrower;
//  5. Compute Savings Plans — highest savings percentage first, ties broken by
//     the lower SP rate;
//  6. the remainder at on-demand rates.
//
// Commitments are charged whether or not usage absorbs them. That is not an
// approximation: it is the whole point. A bill is
// `RI charges + SP commitments + on-demand remainder`, so freeing capacity
// that nothing else absorbs shows up as [Cost.StrandedUSD] rather than as a
// saving.
//
// # Money convention
//
// Hourly USD in float64 — the same convention as the parent pkg/pricing
// package (see pricing.HoursPerMonth). Money is never float32 and never
// compared with ==; every comparison here goes through [Eps]. Rates are
// per-unit-hour; quantities are unit-hours, and are divisible, because AWS
// applies commitments fractionally (a c4.large RI covers 50 % of a c4.xlarge).
//
// # Purity
//
// No AWS SDK, no network, no filesystem beyond an explicit
// [LoadInventoryFile], no clock: callers pass time in. The decision path works
// fully offline from a JSON inventory file. The cloud-facing seam is *defined*
// in sync.go and implemented elsewhere, under a package that may link the SDK.
//
// # Determinism
//
// [Inventory.Bill] is a pure function of the *multiset* of usage lines and
// commitments: it never iterates a map, and it sorts every pool by an
// intrinsic canonical key rather than by input position, so shuffling the
// input cannot change any output. [Cost.Coverage] is returned in that
// canonical order, not input order — match entries by LineID.
package commit

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Eps is the tolerance for money and quantity comparisons. Hourly rates run
// down to ~1e-8 USD (Lambda GB-second pricing), so this is small enough to
// distinguish real rates and large enough to absorb float64 rounding in the
// waterfall's running subtractions.
const Eps = 1e-9

// HoursPerMonth converts hourly to monthly cost. It is the billing-average
// month (8760 h/year ÷ 12) and MUST equal pricing.HoursPerMonth — asserted by
// TestHoursPerMonthMatchesPricing. It is duplicated rather than imported so
// that pkg/pricing stays free to depend on this package later without an
// import cycle.
const HoursPerMonth = 730

// Kind classifies a usage line by product, because commitment eligibility
// differs per product: Compute SPs cover KindEC2, KindFargate and KindLambda,
// EC2 Instance SPs cover only KindEC2, and Reserved Instances cover only
// KindEC2.
//
// KindRDS is outside all three. No Savings Plan of any type covers RDS and no
// EC2 Reserved Instance can match it; a KindRDS line is absorbed by a
// [ReservedDBInstance] and by nothing else. See rds.go.
type Kind string

const (
	KindEC2     Kind = "ec2"
	KindFargate Kind = "fargate"
	KindLambda  Kind = "lambda"
	KindRDS     Kind = "rds"
)

// Platform and tenancy values, normalized. Reserved Instance size flexibility
// requires Linux/UNIX on default tenancy — see [SizeFlexible].
const (
	PlatformLinux  = "Linux/UNIX"
	TenancyDefault = "default"
)

// UsageLine is one hour of homogeneous, divisible billable usage, priced
// before any commitment is applied.
//
// Quantity is in unit-hours: instance-hours for KindEC2, vCPU-hours or
// GB-hours for KindFargate, GB-seconds or millions-of-requests for
// KindLambda. ODRate is USD per unit-hour.
//
// A zero or negative Savings Plans rate means "unknown", not "free": the
// waterfall then applies the conservative fallback documented on
// [Inventory.Bill]. Set SPIneligible for usage no Savings Plan can cover —
// Lambda request charges and Fargate ephemeral storage (§4.4).
type UsageLine struct {
	// ID is the stable identity of the thing being billed (instance ID,
	// workload key, ARN). NetSavings matches before/after lines by ID, so an
	// unstable ID silently weakens the conservative fallback.
	ID           string  `json:"id"`
	Kind         Kind    `json:"kind"`
	Region       string  `json:"region"`
	AZ           string  `json:"az,omitempty"`
	InstanceType string  `json:"instanceType,omitempty"` // "m5.xlarge"; KindEC2 only
	Platform     string  `json:"platform,omitempty"`     // "" ⇒ Linux/UNIX
	Tenancy      string  `json:"tenancy,omitempty"`      // "" ⇒ default
	Unit         string  `json:"unit,omitempty"`         // label only: "vCPU-Hours", "GB-Hours", …
	Quantity     float64 `json:"quantity"`
	ODRate       float64 `json:"odRate"`

	ComputeSPRate float64 `json:"computeSPRate,omitempty"` // ≤0 ⇒ unknown
	EC2SPRate     float64 `json:"ec2SPRate,omitempty"`     // ≤0 ⇒ unknown
	SPIneligible  bool    `json:"spIneligible,omitempty"`

	// Engine and Deployment apply to KindRDS only, where InstanceType holds a
	// DB instance class ("db.r6i.large"). Both change the arithmetic rather
	// than labelling it: Reserved DB Instance size flexibility is gated on the
	// engine, and the deployment topology multiplies the line's normalized
	// units (Multi-AZ instance ×2, Multi-AZ cluster ×3). See rds.go.
	Engine     string        `json:"engine,omitempty"`
	Deployment RDSDeployment `json:"deployment,omitempty"`
}

// Family returns the instance family ("m5" for "m5.xlarge"), or "".
func (l UsageLine) Family() string { return FamilyOf(l.InstanceType) }

// Size returns the instance size ("xlarge" for "m5.xlarge"), or "".
func (l UsageLine) Size() string { return SizeOf(l.InstanceType) }

// canonicalKey is the intrinsic sort key that makes Bill order-independent.
// Two lines with the same key are indistinguishable in every output field.
func (l UsageLine) canonicalKey() string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', 17, 64) }
	return strings.Join([]string{
		l.ID, string(l.Kind), l.Region, l.AZ, l.InstanceType,
		NormalizePlatform(l.Platform), NormalizeTenancy(l.Tenancy), l.Unit,
		NormalizeRDSEngine(l.Engine), string(NormalizeRDSDeployment(l.Deployment)),
		f(l.Quantity), f(l.ODRate), f(l.ComputeSPRate), f(l.EC2SPRate),
		strconv.FormatBool(l.SPIneligible),
	}, "\x00")
}

// Usage is one hour of billable usage across every domain Kilter observes.
// Compute Savings Plans absorb usage account-wide — EC2, Fargate and Lambda
// alike — so a partial Usage understates absorption and therefore overstates
// savings. Bill what you observe, all of it.
type Usage struct {
	Lines []UsageLine `json:"lines"`
}

// OnDemandHourlyUSD is the pre-commitment list price of this usage.
//
// It exists so a UI can show the fantasy beside the fact. Subtracting two of
// these is the exact bug this package was written to prevent: use
// [Inventory.NetSavings] and claim [Assessment.ClaimableHourlyUSD].
func (u Usage) OnDemandHourlyUSD() float64 {
	var total float64
	for _, s := range newStates(u.Lines) { // canonical order ⇒ order-independent sum
		total += s.line.Quantity * s.line.ODRate
	}
	return total
}

// Validate reports the first structurally invalid line. Bill itself never
// fails — it clamps garbage to zero rather than poisoning a total with NaN —
// so call Validate at the boundary where bad data enters, the way
// pricing.Load does for catalogs.
func (u Usage) Validate() error {
	for i, l := range u.Lines {
		switch {
		case l.Kind != KindEC2 && l.Kind != KindFargate && l.Kind != KindLambda && l.Kind != KindRDS:
			return fmt.Errorf("commit: usage line %d (%q): unknown kind %q", i, l.ID, l.Kind)
		case l.Kind == KindEC2 && l.InstanceType == "":
			return fmt.Errorf("commit: usage line %d (%q): ec2 line needs an instanceType", i, l.ID)
		case !finite(l.Quantity) || l.Quantity < 0:
			return fmt.Errorf("commit: usage line %d (%q): bad quantity %v", i, l.ID, l.Quantity)
		case !finite(l.ODRate) || l.ODRate < 0:
			return fmt.Errorf("commit: usage line %d (%q): bad on-demand rate %v", i, l.ID, l.ODRate)
		case !finite(l.ComputeSPRate) || !finite(l.EC2SPRate):
			return fmt.Errorf("commit: usage line %d (%q): non-finite savings-plan rate", i, l.ID)
		case l.Kind == KindRDS:
			// Last clause on purpose: the checks above apply to RDS too, and a
			// Go switch case does not fall through.
			if err := l.validateRDS(); err != nil {
				return fmt.Errorf("commit: usage line %d (%q): %w", i, l.ID, err)
			}
		}
	}
	return nil
}

// ReservedInstance is one Reserved Instance purchase, as returned by
// ec2:DescribeReservedInstances.
//
// EffectiveHourlyUSD is the amortized all-in rate for ONE reservation:
// hourly recurring charge plus upfront ÷ term hours. It is charged for every
// hour of the term whether or not usage absorbs it, which is what makes
// stranding visible.
//
// OfferingClass is recorded because convertible RIs offer an exchange path a
// human may take; it deliberately does not affect billing, matching AWS
// ("The offering class … does not affect how the billing discount is
// applied"). Kilter may note an exchange, never execute one.
type ReservedInstance struct {
	ID                 string    `json:"id"`
	Count              int       `json:"count"`
	InstanceType       string    `json:"instanceType"`
	Region             string    `json:"region"`
	AZ                 string    `json:"az,omitempty"` // "" ⇒ regional; set ⇒ zonal
	Platform           string    `json:"platform,omitempty"`
	Tenancy            string    `json:"tenancy,omitempty"`
	OfferingClass      string    `json:"offeringClass,omitempty"` // standard | convertible
	EffectiveHourlyUSD float64   `json:"effectiveHourlyUSD"`
	Expires            time.Time `json:"expires,omitempty"`
}

// Zonal reports whether this RI reserves capacity in one Availability Zone.
// Zonal RIs match exactly and have no size flexibility.
func (r ReservedInstance) Zonal() bool { return r.AZ != "" }

// Family returns the RI's instance family.
func (r ReservedInstance) Family() string { return FamilyOf(r.InstanceType) }

// SizeFlexible reports whether this RI's discount floats across sizes within
// its family via normalization units. Per apply_ri.html, that requires a
// regional RI on Linux/UNIX with default tenancy, in a family that is not on
// AWS's exclusion list, whose size has a known normalization factor.
func (r ReservedInstance) SizeFlexible() bool {
	if r.Zonal() || NormalizePlatform(r.Platform) != PlatformLinux ||
		NormalizeTenancy(r.Tenancy) != TenancyDefault || SizeFlexExcluded(r.Family()) {
		return false
	}
	_, ok := InstanceUnits(r.InstanceType)
	return ok
}

// SavingsPlanType distinguishes the two commitment shapes Kilter models.
type SavingsPlanType string

const (
	// SPCompute applies to EC2 in any family/size/region plus Fargate and
	// Lambda. Broadest, so applied last.
	SPCompute SavingsPlanType = "compute"
	// SPEC2Instance is locked to one instance family in one region and never
	// covers Fargate or Lambda. Narrower, so applied first.
	SPEC2Instance SavingsPlanType = "ec2-instance"
)

// SavingsPlan is one commitment, as returned by savingsplans:DescribeSavingsPlans.
// The commitment is use-it-or-lose-it per hour: an unused dollar in this hour
// does not carry into the next one.
type SavingsPlan struct {
	ID                   string          `json:"id"`
	Type                 SavingsPlanType `json:"type"`
	CommitmentUSDPerHour float64         `json:"commitmentUSDPerHour"`
	Region               string          `json:"region,omitempty"` // SPEC2Instance only
	Family               string          `json:"family,omitempty"` // SPEC2Instance only
	Expires              time.Time       `json:"expires,omitempty"`
}

// Inventory is the account's commitment position. The zero value (or a nil
// *Inventory) is valid and prices everything at on-demand — an account with no
// commitments, where gross savings and net savings coincide.
type Inventory struct {
	RIs          []ReservedInstance `json:"reservedInstances,omitempty"`
	SavingsPlans []SavingsPlan      `json:"savingsPlans,omitempty"`
	// ReservedDBs are Reserved DB Instances — the RDS commitment product.
	// They are a separate list rather than a flag on RIs because they match on
	// different keys entirely (instance class type and engine, not family and
	// platform) and cover a disjoint set of usage. See rds.go.
	ReservedDBs []ReservedDBInstance `json:"reservedDBInstances,omitempty"`
	FetchedAt   time.Time            `json:"fetchedAt,omitempty"`
}

// Active returns the subset of the inventory still in force at t. Commitments
// with a zero Expires are treated as open-ended and always retained. This is
// how a suppression lapses: re-bill against Active(now) and a recommendation
// blocked by an expired RI stops being blocked, with no state to clean up.
func (inv *Inventory) Active(t time.Time) *Inventory {
	out := &Inventory{FetchedAt: inv.fetchedAt()}
	if inv == nil {
		return out
	}
	for _, r := range inv.RIs {
		if r.Expires.IsZero() || r.Expires.After(t) {
			out.RIs = append(out.RIs, r)
		}
	}
	for _, s := range inv.SavingsPlans {
		if s.Expires.IsZero() || s.Expires.After(t) {
			out.SavingsPlans = append(out.SavingsPlans, s)
		}
	}
	out.ReservedDBs = inv.activeReservedDBs(t)
	return out
}

func (inv *Inventory) fetchedAt() time.Time {
	if inv == nil {
		return time.Time{}
	}
	return inv.FetchedAt
}

// Validate rejects an inventory that would silently skew every bill built on
// it: non-finite or negative money, non-positive counts, unknown plan types,
// and EC2 Instance Savings Plans without the region/family scope that defines
// what they may cover.
func (inv *Inventory) Validate() error {
	if inv == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, r := range inv.RIs {
		switch {
		case r.Count <= 0:
			return fmt.Errorf("commit: reserved instance %d (%q): count must be > 0, got %d", i, r.ID, r.Count)
		case r.InstanceType == "":
			return fmt.Errorf("commit: reserved instance %d (%q): instanceType required", i, r.ID)
		case r.Region == "":
			return fmt.Errorf("commit: reserved instance %d (%q): region required", i, r.ID)
		case !finite(r.EffectiveHourlyUSD) || r.EffectiveHourlyUSD < 0:
			return fmt.Errorf("commit: reserved instance %d (%q): bad effectiveHourlyUSD %v", i, r.ID, r.EffectiveHourlyUSD)
		}
		if r.ID != "" {
			if seen["ri/"+r.ID] {
				// Duplicate IDs would double-count committed spend and make
				// per-commitment stranding attribution ambiguous.
				return fmt.Errorf("commit: duplicate reserved instance id %q", r.ID)
			}
			seen["ri/"+r.ID] = true
		}
	}
	for i, s := range inv.SavingsPlans {
		switch {
		case s.Type != SPCompute && s.Type != SPEC2Instance:
			return fmt.Errorf("commit: savings plan %d (%q): unknown type %q", i, s.ID, s.Type)
		case !finite(s.CommitmentUSDPerHour) || s.CommitmentUSDPerHour < 0:
			return fmt.Errorf("commit: savings plan %d (%q): bad commitment %v", i, s.ID, s.CommitmentUSDPerHour)
		case s.Type == SPEC2Instance && (s.Region == "" || s.Family == ""):
			return fmt.Errorf("commit: savings plan %d (%q): ec2-instance plan needs region and family", i, s.ID)
		}
		if s.ID != "" {
			if seen["sp/"+s.ID] {
				return fmt.Errorf("commit: duplicate savings plan id %q", s.ID)
			}
			seen["sp/"+s.ID] = true
		}
	}
	return inv.validateReservedDBs()
}

// LoadInventory parses a commitment inventory from JSON and validates it.
// Unknown fields are rejected: a typo'd key would otherwise become a silently
// missing commitment, which reads as "no stranding" and re-opens the trap.
func LoadInventory(r io.Reader) (*Inventory, error) {
	var inv Inventory
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("commit: parse inventory: %w", err)
	}
	if err := inv.Validate(); err != nil {
		return nil, err
	}
	return &inv, nil
}

// LoadInventoryFile loads the inventory `kilter pricing sync-commitments`
// writes. This is the whole offline path: no credentials, no network.
func LoadInventoryFile(path string) (*Inventory, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return LoadInventory(f)
}

// WriteInventory serializes an inventory as indented JSON. Output is
// deterministic for a given inventory: fields are struct-ordered and slices
// keep their order, so a re-sync with no change produces no diff.
func WriteInventory(w io.Writer, inv *Inventory) error {
	if err := inv.Validate(); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if inv == nil {
		inv = &Inventory{}
	}
	return enc.Encode(inv)
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// sane clamps a money or quantity input to a usable value. Bill must not
// propagate NaN or ±Inf into a total (that silently destroys every number
// downstream), and negative money is never meaningful here. Callers that want
// to hear about bad data call Validate.
func sane(f float64) float64 {
	if !finite(f) || f < 0 {
		return 0
	}
	return f
}
