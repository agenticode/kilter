package commit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// The seams `kilter pricing sync-commitments` will satisfy.
//
// They are DEFINED here and implemented elsewhere — in a package that may link
// the AWS SDK, the way pkg/pricing/awssync does for the catalog and
// pkg/provider does for EKS. Nothing in this package implements them, calls
// AWS, or opens a socket: cloud I/O belongs in actuation, never in the
// decision path.
//
// Credentials are optional by design. The sync command writes JSON; the
// decision path reads it with [LoadInventoryFile] and [LoadRateTableFile] and
// works fully offline. With no inventory file at all, a nil *Inventory prices
// everything at on-demand, which is exactly right for an account that holds no
// commitments.

// CommitmentSource fetches the account's commitment position.
//
// The AWS implementation is ec2:DescribeReservedInstances (inventory, term,
// offering class, scope) plus savingsplans:DescribeSavingsPlans (commitment
// $/h, type, and the region/family scope of EC2 Instance plans). Implementers
// must return a validated [Inventory] — see [Inventory.Validate] — and must
// paginate to exhaustion: a truncated inventory reads as "less commitment
// than we have", which understates stranding and re-opens the §4.4 trap.
type CommitmentSource interface {
	FetchCommitments(ctx context.Context) (*Inventory, error)
}

// RateSource fetches Savings Plans rates per usage type for one region.
//
// The AWS implementation is savingsplans:DescribeSavingsPlansOfferingRates —
// an action name the design doc flags as [unverified] (§4.4). Kilter is
// therefore built to work without it: when a rate is missing, [Inventory.Bill]
// applies the conservative fallback and under-claims rather than guessing.
// An implementation that cannot resolve a rate must omit it, never substitute
// the on-demand rate — a wrong rate is worse than an absent one, because the
// fallback is only safe when absence is honest.
type RateSource interface {
	FetchRates(ctx context.Context, region string) (*RateTable, error)
}

// SPRates are the two Savings Plans rates for one usage type, in USD per
// unit-hour. A zero value means "not offered / unknown".
type SPRates struct {
	ComputeUSD     float64 `json:"computeUSD,omitempty"`
	EC2InstanceUSD float64 `json:"ec2InstanceUSD,omitempty"`
}

// RateTable maps usage types to their Savings Plans rates. The zero value is
// an empty table: every lookup misses and every line takes the conservative
// fallback.
type RateTable struct {
	Rates map[string]SPRates `json:"rates,omitempty"`
}

// RateKey is the usage-type key for a line: everything that can change the
// Savings Plans rate, and nothing that cannot.
func RateKey(l UsageLine) string {
	parts := []string{string(l.Kind), strings.ToLower(l.Region)}
	if l.Kind == KindEC2 {
		parts = append(parts, strings.ToLower(l.InstanceType),
			NormalizePlatform(l.Platform), NormalizeTenancy(l.Tenancy))
	} else {
		parts = append(parts, strings.ToLower(l.Unit))
	}
	return strings.Join(parts, "|")
}

// Set records the rates for a usage-type key.
func (t *RateTable) Set(key string, r SPRates) {
	if t.Rates == nil {
		t.Rates = map[string]SPRates{}
	}
	t.Rates[key] = r
}

// Lookup returns the rates for a line, if the table has them.
func (t *RateTable) Lookup(l UsageLine) (SPRates, bool) {
	if t == nil || t.Rates == nil {
		return SPRates{}, false
	}
	r, ok := t.Rates[RateKey(l)]
	return r, ok
}

// Apply returns a copy of the usage with Savings Plans rates filled in from
// the table. Lines the table does not know keep their zero rates and take the
// conservative fallback in [Inventory.Bill]; rates already set on a line win,
// so a caller can pin a known rate the table lacks. The input is not mutated,
// and line order is preserved.
func (t *RateTable) Apply(u Usage) Usage {
	out := Usage{Lines: make([]UsageLine, len(u.Lines))}
	copy(out.Lines, u.Lines)
	for i := range out.Lines {
		r, ok := t.Lookup(out.Lines[i])
		if !ok || out.Lines[i].SPIneligible {
			continue
		}
		if out.Lines[i].ComputeSPRate <= 0 {
			out.Lines[i].ComputeSPRate = r.ComputeUSD
		}
		if out.Lines[i].EC2SPRate <= 0 {
			out.Lines[i].EC2SPRate = r.EC2InstanceUSD
		}
	}
	return out
}

// LoadRateTable parses a rate table from JSON, rejecting unknown fields and
// non-finite or negative rates.
func LoadRateTable(r io.Reader) (*RateTable, error) {
	var t RateTable
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("commit: parse rate table: %w", err)
	}
	for k, v := range t.Rates {
		if !finite(v.ComputeUSD) || v.ComputeUSD < 0 || !finite(v.EC2InstanceUSD) || v.EC2InstanceUSD < 0 {
			return nil, fmt.Errorf("commit: rate table entry %q: bad rate %+v", k, v)
		}
	}
	return &t, nil
}

// LoadRateTableFile loads a rate table from disk. A missing file is not an
// error condition this package invents a default for — the caller decides,
// and nil is a valid, fully conservative table.
func LoadRateTableFile(path string) (*RateTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return LoadRateTable(f)
}

// WriteRateTable serializes a rate table. encoding/json sorts map keys, so the
// output is byte-stable for a given table.
func WriteRateTable(w io.Writer, t *RateTable) error {
	if t == nil {
		t = &RateTable{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(t)
}
