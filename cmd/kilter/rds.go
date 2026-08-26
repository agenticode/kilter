package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	krds "github.com/agenticode/kilter/pkg/rds"
)

// The RDS wiring over a RECORDED account.
//
// pkg/rds/FINDINGS.md §6 owes cmd/ four things: the domain kind (landed in
// pkg/domain), an SDK adapter over the read seams, the collection loop, and
// the rate override. All four are now wired — the adapter landed in
// pkg/provider (PR#45) and cmd/kilter/rdslive.go drives it — and this file
// keeps the half that needs no account.
//
// It is not a stub and it never was. `rds.Fixture` implements the three
// collection seams with real pagination, real truncation and real
// empty-account behaviour, and it is exported for exactly this reason — "the
// seams are the contract, and a contract nobody outside the package can
// exercise is not a contract". So --rds-fixture drives the REAL collector:
// rds.NewCollector over the recorded account, rds.Collector.Collect, the real
// window clamp, the real GetMetricData batching and ID routing. `rds`'s
// EnvelopeFixture does the same for the U13 modification seam, so --rds-parity
// is exercisable without an AWS account too.
//
// No credential is read, no ~/.aws is opened, and no network call is made on
// THIS path. `--rds-region` is the path that does, and it is a sibling rather
// than a replacement: every test in this package drives the recorded one.

// rdsFixtureFile is the on-disk shape of a recorded RDS account.
//
// It is a cmd/-side projection of `rds.Fixture` rather than that type
// directly, and the mismatch that forced it is worth naming: `rds.Fixture`
// carries `error` fields (InstancesErr, TagsErr, …) and a `Calls` counter
// struct, none of which have a JSON representation. Decoding straight into it
// would give a file format with four fields that silently cannot be set and
// one that pretends to be input. This projection carries the DATA half only,
// with explicit json tags, and turns the two seam-absence cases into the
// booleans the IAM table in §6.2 actually describes.
type rdsFixtureFile struct {
	// Instances is the recorded rds:DescribeDBInstances inventory.
	Instances []krds.DBInstanceRecord `json:"instances,omitempty"`
	// Clusters is the recorded rds:DescribeDBClusters inventory. It is read
	// for one reason: to tell an Aurora cluster from a Multi-AZ DB cluster
	// without inferring it from a member's engine string (§5.3).
	Clusters []krds.DBClusterRecord `json:"clusters,omitempty"`
	// Tags maps a DB instance ARN to its rds:ListTagsForResource answer.
	Tags map[string]map[string]string `json:"tags,omitempty"`
	// Metrics maps "<dbInstanceIdentifier>/<metricName>" to datapoints.
	Metrics map[string][]krds.Point `json:"metrics,omitempty"`
	// Reservations is the recorded rds:DescribeReservedDBInstances inventory.
	Reservations []krds.ReservedDBInstanceRecord `json:"reservations,omitempty"`
	// PageSize splits every paginated response; 0 means one page.
	PageSize int `json:"pageSize,omitempty"`
	// DropResults omits the first N results from every GetMetricData page,
	// reproducing a TRUNCATED response. A missing result is "we were not
	// told", never "the metric is empty", and this is the knob that proves an
	// idle verdict cannot be manufactured out of silence.
	DropResults int `json:"dropResults,omitempty"`

	// NoMetricsAPI models a caller holding rds:Describe* and NOT
	// cloudwatch:GetMetricData. §6.2: nil ⇒ every instance refuses with
	// no-metric-evidence. That is a complete report, not a failed one.
	NoMetricsAPI bool `json:"noMetricsAPI,omitempty"`
	// NoCommitmentAPI models a caller without
	// rds:DescribeReservedDBInstances. §6.2: nil ⇒ net == gross, which
	// under-claims and can never invent a saving.
	NoCommitmentAPI bool `json:"noCommitmentAPI,omitempty"`

	// --- the U13 modification seam, read only under --rds-parity ----------

	// StorageOptions is the recorded rds:DescribeValidDBInstanceModifications
	// answer per DBInstanceIdentifier: the ranges AWS says this instance can
	// be provisioned within. An instance absent from this map has an UNKNOWN
	// envelope, not an unlimited one, and every provisioning proposal for it
	// is refused by name.
	StorageOptions map[string][]krds.ValidStorageOptionRecord `json:"storageOptions,omitempty"`
	// Events is the recorded rds:DescribeEvents answer per
	// DBInstanceIdentifier. It is what the four-storage-modifications-per-24-
	// hours limit is evaluated from, and an instance absent from this map has
	// an empty history that WAS read — which is not the same as a history that
	// could not be read (NoEnvelopeAPI).
	Events map[string][]krds.EventRecord `json:"events,omitempty"`
	// NoEnvelopeAPI models a caller holding rds:Describe* and NOT
	// rds:DescribeValidDBInstanceModifications. nil ⇒ every envelope is
	// unknown and every provisioning proposal refuses with
	// provisioning-envelope-unknown. That is a complete report, not a failed
	// one, and it is a DIFFERENT report from one where the seam answered and
	// named no ceiling.
	NoEnvelopeAPI bool `json:"noEnvelopeAPI,omitempty"`
}

// collectRDS runs the real collector over a recorded account and returns the
// native snapshot. It is collectRDSFixture without the U13 envelope, kept as
// the narrow entry point the generic-seam test drives.
func collectRDS(ctx context.Context, path, scope, region string, now time.Time, span time.Duration) (*krds.Snapshot, []string, error) {
	snap, _, warns, err := collectRDSFixture(ctx, path,
		rdsCollectOptions{Scope: scope, Region: region, Now: now, Span: span})
	return snap, warns, err
}

// collectRDSFixture runs the real collector — and, under --rds-parity, the
// real envelope collector — over a recorded account.
//
// The window is [now-span, now] and is then CLAMPED by the collector, because
// 1-minute CloudWatch datapoints live 15 days: a snapshot that claims a 30-day
// window and holds 15 days of data is a lie told by omission, and every
// downstream "insufficient window" gate reads the claim rather than the data.
// The clamp is why c.Window() is rendered and the request is not.
func collectRDSFixture(ctx context.Context, path string, opts rdsCollectOptions) (
	*krds.Snapshot, []krds.Envelope, []string, error) {

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--rds-fixture: %w", err)
	}
	var ff rdsFixtureFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ff); err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}

	fx := &krds.Fixture{
		Instances:    ff.Instances,
		Clusters:     ff.Clusters,
		Tags:         ff.Tags,
		Metrics:      ff.Metrics,
		Reservations: ff.Reservations,
		PageSize:     ff.PageSize,
		DropResults:  ff.DropResults,
	}

	cfg := krds.DefaultCollectorConfig(krds.Window{Start: opts.Now.Add(-opts.Span), End: opts.Now})
	cfg.Scope, cfg.Region = opts.Scope, opts.Region

	// The three seams. Two of them are optional and their absence is a
	// DIFFERENT report rather than a failure — that is the whole reason
	// pkg/rds declares them separately instead of as one client interface.
	var metrics krds.MetricsAPI
	var reserved krds.CommitmentAPI
	if !ff.NoMetricsAPI {
		metrics = fx
	}
	if !ff.NoCommitmentAPI {
		reserved = fx
	}

	c, err := krds.NewCollector(fx, metrics, reserved, cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	snap, err := c.Collect(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", path, err)
	}

	var warnings []string
	if got := c.Window(); got != cfg.Window {
		warnings = append(warnings, fmt.Sprintf(
			"%s: observation window clamped to %s (1-minute CloudWatch datapoints live %s)",
			path, got.String(), krds.RetentionAtOneMinute))
	}
	for _, w := range snap.Warnings {
		warnings = append(warnings, path+": "+w)
	}

	// The fourth seam, read only when --rds-parity asked for it. NoEnvelopeAPI
	// hands the collector a nil interface, which is legal and yields a wholly
	// unknown envelope set — the recorded form of a caller who holds
	// rds:Describe* and not rds:DescribeValidDBInstanceModifications.
	var envAPI krds.ModificationEnvelopeAPI
	if !ff.NoEnvelopeAPI {
		envAPI = &krds.EnvelopeFixture{
			Options: ff.StorageOptions, Events: ff.Events, PageSize: ff.PageSize,
		}
	}
	envs, ewarns, err := collectRDSEnvelopes(ctx, opts, envAPI, rdsIdentifiers(snap), path)
	warnings = append(warnings, ewarns...)
	if err != nil {
		return nil, nil, warnings, fmt.Errorf("%s: %w", path, err)
	}
	return snap, envs, warnings, nil
}

// loadRDSRates resolves the rate card.
//
// Layering, not replacement: DefaultRates().Merge(loaded) lets an operator
// supply the SQL Server and Oracle rows this package ships none of, without
// restating every open-source row. Every loaded row is stamped
// `operator-supplied` by pkg/rds and is therefore claimable; every shipped row
// is `unverified` and can size a fact and never a saving. That asymmetry is
// the intended, loud failure mode — until somebody who can see their own
// invoice supplies a file, no RDS dollar is claimable.
func loadRDSRates(path string) (krds.RateCard, error) {
	base := krds.DefaultRates()
	if path == "" {
		return base, nil
	}
	over, err := krds.LoadRatesFile(path)
	if err != nil {
		return krds.RateCard{}, fmt.Errorf("--rds-rates %s: %w", path, err)
	}
	return base.Merge(over), nil
}
