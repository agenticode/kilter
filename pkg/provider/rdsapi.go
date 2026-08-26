package provider

// The live RDS SDK adapter: a field copy, and the four places where a field
// copy is not enough.
//
// pkg/rds declares four read seams over plain Go structs whose field names are
// the AWS API's own (pkg/rds/collect.go, pkg/rds/parity_envelope.go). It
// imports no SDK, on purpose — its decision path has to link into an air-gapped
// binary. This file is the other half: *rds.Client on one side, those structs
// on the other, and nothing in between that decides anything.
//
// Two conversions are already done INSIDE pkg/rds and are deliberately NOT
// redone here (pkg/rds/FINDINGS.md §6.2):
//
//   - reservation amortization — EffectiveHourly = UsagePrice + FixedPrice ÷
//     term hours, plus the active/payment-pending filter (reservationFromRecord)
//   - deployment topology — MultiAZ → commit.RDSMultiAZInstance
//     (DBInstance.Deployment)
//
// So FixedPrice, UsagePrice, Duration and MultiAZ are copied raw. Amortizing
// or classifying them here would give the tree two implementations of an
// arithmetic that must have exactly one.
//
// What this file DOES decide, because a field copy cannot avoid it, is what an
// unset SDK pointer means. `*int32` → `int32` through a nil check that defaults
// to 0 turns "AWS did not say" into "AWS said zero", and downstream that reads
// as a measurement. Every nilable field's decision is recorded in
// RDS-ADAPTER-FINDINGS.md §4 and tested in rdsapi_test.go; the two that
// actually change a verdict are:
//
//   - ValidStorageOptions with no readable ceiling is OMITTED, so the envelope
//     stays Known=false. Emitting it would make StorageEnvelope.Known true with
//     MaxIOPS==0, and pkg/rds/parity.go:487 reads MaxIOPS==0 as "no ceiling to
//     enforce" — an unknown ceiling would silently become an unlimited one.
//   - a GetMetricData result with no StatusCode is reported as PartialData
//     (cloudwatchapi.go), because pkg/rds defaults an empty status to Complete.
//
// Facts that neither seam struct has a field for — an instance with no
// identifier, a tag with no key, an event with no date — go to Notes(), which
// cmd/ renders beside the snapshot's own warnings. A degradation nobody can
// see is a degradation that did not happen.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsrds "github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/smithy-go"

	krds "github.com/agenticode/kilter/pkg/rds"
)

// DefaultRDSCallTimeout bounds one rds: API call. Every operation here is a
// single describe against a control-plane API that answers in well under a
// second; 30 s is generous enough that a slow region is not mistaken for a
// broken one, and short enough that a hung call cannot hold a collection open
// past its page budget.
const DefaultRDSCallTimeout = 30 * time.Second

// rdsSDK is the minimal rds: surface this adapter needs, satisfied by
// *rds.Client and by test fakes. It is six operations, all of them GETs: this
// interface IS the write surface audit, and it is empty.
type rdsSDK interface {
	DescribeDBInstances(ctx context.Context, in *awsrds.DescribeDBInstancesInput,
		opts ...func(*awsrds.Options)) (*awsrds.DescribeDBInstancesOutput, error)
	DescribeDBClusters(ctx context.Context, in *awsrds.DescribeDBClustersInput,
		opts ...func(*awsrds.Options)) (*awsrds.DescribeDBClustersOutput, error)
	ListTagsForResource(ctx context.Context, in *awsrds.ListTagsForResourceInput,
		opts ...func(*awsrds.Options)) (*awsrds.ListTagsForResourceOutput, error)
	DescribeReservedDBInstances(ctx context.Context, in *awsrds.DescribeReservedDBInstancesInput,
		opts ...func(*awsrds.Options)) (*awsrds.DescribeReservedDBInstancesOutput, error)
	DescribeValidDBInstanceModifications(ctx context.Context,
		in *awsrds.DescribeValidDBInstanceModificationsInput,
		opts ...func(*awsrds.Options)) (*awsrds.DescribeValidDBInstanceModificationsOutput, error)
	DescribeEvents(ctx context.Context, in *awsrds.DescribeEventsInput,
		opts ...func(*awsrds.Options)) (*awsrds.DescribeEventsOutput, error)
}

// RDSAPI adapts *rds.Client to the three rds:-side seams pkg/rds declares.
//
// One concrete type implements all three because one credential answers all
// three, exactly as rds.Fixture does. They stay SEPARATE interfaces at the
// pkg/rds boundary for the reason pkg/rds gives: a caller may hold
// rds:DescribeDBInstances without rds:DescribeReservedDBInstances, and the
// right behaviour then is a complete report with net == gross rather than no
// report at all. cmd/ expresses that by passing nil for the optional
// parameter, NOT by passing an RDSAPI that fails — see
// RDS-ADAPTER-FINDINGS.md §5.
//
// Pagination is per-call and token-shaped, because that is the shape of the
// seam: pkg/rds owns the page loop and its page budget, and this adapter's job
// is to propagate Marker faithfully in both directions. Swallowing a Marker
// here would silently truncate an inventory into a report that reads as
// complete.
type RDSAPI struct {
	api     rdsSDK
	region  string
	timeout time.Duration
	notes   noteSet
}

var (
	_ krds.InventoryAPI            = (*RDSAPI)(nil)
	_ krds.CommitmentAPI           = (*RDSAPI)(nil)
	_ krds.ModificationEnvelopeAPI = (*RDSAPI)(nil)
)

// NewRDSAPI loads AWS credentials from the environment (IRSA, instance
// profile, env vars, shared config) and targets one region.
//
// The region is required rather than inherited from the ambient config, and
// not for tidiness: CollectorConfig.Region is what stamps DBInstance.Region and
// therefore which rate-card row prices every instance. A client talking to
// us-west-2 under a config that says us-east-1 produces a report whose every
// dollar is confidently wrong. Read Region() back and pass the SAME value to
// CollectorConfig.Region.
func NewRDSAPI(ctx context.Context, region string) (*RDSAPI, error) {
	region = strings.TrimSpace(region)
	if region == "" {
		return nil, fmt.Errorf("provider rds: region required: it stamps DBInstance.Region and so " +
			"selects the rate-card row that prices every instance")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("provider rds: load aws config: %w", err)
	}
	return newRDSAPI(awsrds.NewFromConfig(cfg), region), nil
}

// newRDSAPI is the test seam.
func newRDSAPI(client rdsSDK, region string) *RDSAPI {
	return &RDSAPI{api: client, region: region, timeout: DefaultRDSCallTimeout}
}

// Region is the region this adapter reads, to be handed to
// CollectorConfig.Region unchanged.
func (a *RDSAPI) Region() string { return a.region }

// SetCallTimeout overrides the per-call deadline. A non-positive value
// restores [DefaultRDSCallTimeout].
func (a *RDSAPI) SetCallTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultRDSCallTimeout
	}
	a.timeout = d
}

// Notes returns what this adapter observed that the seam structs have no field
// for, deduplicated and in a stable order. cmd/ must render them beside
// Snapshot.Warnings; see RDS-ADAPTER-FINDINGS.md §6.
func (a *RDSAPI) Notes() []string { return a.notes.list() }

// call bounds one SDK call. The parent context still cancels — this only ever
// shortens the deadline, never extends it.
func (a *RDSAPI) call(ctx context.Context) (context.Context, context.CancelFunc) {
	d := a.timeout
	if d <= 0 {
		d = DefaultRDSCallTimeout
	}
	return context.WithTimeout(ctx, d)
}

// --- InventoryAPI ----------------------------------------------------------

// DescribeDBInstances implements [krds.InventoryAPI]: one page per call, with
// the caller's Marker in and AWS's Marker out.
func (a *RDSAPI) DescribeDBInstances(ctx context.Context,
	in *krds.DescribeDBInstancesInput) (*krds.DescribeDBInstancesOutput, error) {

	if in == nil {
		in = &krds.DescribeDBInstancesInput{}
	}
	cctx, cancel := a.call(ctx)
	defer cancel()
	res, err := a.api.DescribeDBInstances(cctx, &awsrds.DescribeDBInstancesInput{
		Marker: strPtr(in.Marker),
		// MaxRecords is passed only when the caller asked for one. Sending 0
		// would be rejected by AWS (the range is 20–100) on a request the
		// caller never made.
		MaxRecords: i32Ptr(in.MaxRecords),
	})
	if err != nil {
		return nil, fmt.Errorf("provider rds: DescribeDBInstances: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	out := &krds.DescribeDBInstancesOutput{Marker: str(res.Marker)}
	for _, r := range res.DBInstances {
		out.DBInstances = append(out.DBInstances, a.dbInstanceRecord(r))
	}
	return out, nil
}

func (a *RDSAPI) dbInstanceRecord(in rdstypes.DBInstance) krds.DBInstanceRecord {
	r := krds.DBInstanceRecord{
		DBInstanceIdentifier: str(in.DBInstanceIdentifier),
		DBInstanceArn:        str(in.DBInstanceArn),
		DBInstanceClass:      str(in.DBInstanceClass),
		DBInstanceStatus:     str(in.DBInstanceStatus),
		Engine:               str(in.Engine),
		EngineVersion:        str(in.EngineVersion),
		LicenseModel:         str(in.LicenseModel),

		// MultiAZ nil ⇒ false ⇒ DBInstance.Deployment() reports Single-AZ,
		// a ×1 rather than a ×2 multiplier. The destination field is a bool
		// and cannot hold "unknown"; false is the under-stating direction,
		// which under-states the instance line and every saving derived from
		// it. See RDS-ADAPTER-FINDINGS.md §4.
		MultiAZ:             bval(in.MultiAZ),
		DBClusterIdentifier: str(in.DBClusterIdentifier),
		AvailabilityZone:    str(in.AvailabilityZone),

		ReadReplicaSourceDBInstanceIdentifier: str(in.ReadReplicaSourceDBInstanceIdentifier),
		ReadReplicaDBInstanceIdentifiers:      nonEmpty(in.ReadReplicaDBInstanceIdentifiers),

		// AllocatedStorage and MaxAllocatedStorage widen *int32 → int64; no
		// value RDS issues comes near the int32 ceiling, so the widening is
		// lossless. A nil MaxAllocatedStorage is AWS's own encoding of
		// "storage autoscaling is off", which is what 0 means here too.
		AllocatedStorage:    int64(i32(in.AllocatedStorage)),
		MaxAllocatedStorage: int64(i32(in.MaxAllocatedStorage)),
		StorageType:         str(in.StorageType),
		// Iops and StorageThroughput are absent on gp2/standard because they
		// are not provisionable there; 0 is the honest reading of that
		// absence, and pkg/rds treats 0 as "not provisioned", not as
		// "measured zero".
		Iops:              i32(in.Iops),
		StorageThroughput: i32(in.StorageThroughput),

		InstanceCreateTime: tval(in.InstanceCreateTime),
		TagList:            a.tagMap(in.TagList, str(in.DBInstanceArn)),
	}
	// pkg/rds keys an instance by ARN, falling back to the identifier, and
	// silently skips a record with neither (collect.go recordID). It is
	// silent because the seam has nowhere to say it; say it here.
	if r.DBInstanceArn == "" && r.DBInstanceIdentifier == "" {
		a.notes.add("rds:DescribeDBInstances returned a DB instance with neither DBInstanceArn nor " +
			"DBInstanceIdentifier; it cannot be addressed and is ABSENT from this report rather than " +
			"reported as having no findings")
	}
	return r
}

func (a *RDSAPI) tagMap(in []rdstypes.Tag, owner string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for _, t := range in {
		k := str(t.Key)
		if k == "" {
			a.notes.add("rds returned a tag with no key on %s; it is dropped, and if it was %s the "+
				"opt-out guardrail would not be honoured", orUnnamed(owner), krds.TagKilterMode)
			continue
		}
		// A nil Value is AWS's encoding of an empty-valued tag, which is a
		// legal RDS tag and is not "off" for the kilter.dev/mode guardrail.
		out[k] = str(t.Value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DescribeDBClusters implements [krds.InventoryAPI]. pkg/rds reads clusters
// for one reason — to tell an Aurora cluster from a Multi-AZ DB cluster
// without inferring it from a member's engine string — so the cluster's own
// Engine is the load-bearing field here.
func (a *RDSAPI) DescribeDBClusters(ctx context.Context,
	in *krds.DescribeDBClustersInput) (*krds.DescribeDBClustersOutput, error) {

	if in == nil {
		in = &krds.DescribeDBClustersInput{}
	}
	cctx, cancel := a.call(ctx)
	defer cancel()
	res, err := a.api.DescribeDBClusters(cctx, &awsrds.DescribeDBClustersInput{
		Marker: strPtr(in.Marker),
	})
	if err != nil {
		return nil, fmt.Errorf("provider rds: DescribeDBClusters: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	out := &krds.DescribeDBClustersOutput{Marker: str(res.Marker)}
	for _, c := range res.DBClusters {
		rec := krds.DBClusterRecord{
			DBClusterIdentifier: str(c.DBClusterIdentifier),
			DBClusterArn:        str(c.DBClusterArn),
			Engine:              str(c.Engine),
			EngineMode:          str(c.EngineMode),
		}
		for _, m := range c.DBClusterMembers {
			if id := str(m.DBInstanceIdentifier); id != "" {
				rec.DBClusterMembers = append(rec.DBClusterMembers, id)
			}
		}
		// The ACU bounds are carried so the Aurora refusal can name the lever
		// a future unit would look at; pkg/rds never does arithmetic with
		// them, so a nil scaling configuration (every non-Serverless-v2
		// cluster) leaving them at 0 costs nothing.
		if sv2 := c.ServerlessV2ScalingConfiguration; sv2 != nil {
			rec.ServerlessV2MinCapacity = f64(sv2.MinCapacity)
			rec.ServerlessV2MaxCapacity = f64(sv2.MaxCapacity)
		}
		out.DBClusters = append(out.DBClusters, rec)
	}
	return out, nil
}

// ListTagsForResource implements [krds.InventoryAPI]. This is the required
// third operation: without it the kilter.dev/mode opt-out is unreachable and
// an operator who tagged a database to be left alone would not be obeyed.
func (a *RDSAPI) ListTagsForResource(ctx context.Context,
	in *krds.ListTagsForResourceInput) (*krds.ListTagsForResourceOutput, error) {

	if in == nil || strings.TrimSpace(in.ResourceName) == "" {
		// Refused client-side rather than sent: AWS would answer this with an
		// InvalidParameterValue that reads like a permissions problem.
		return nil, fmt.Errorf("provider rds: ListTagsForResource needs a resource ARN")
	}
	cctx, cancel := a.call(ctx)
	defer cancel()
	res, err := a.api.ListTagsForResource(cctx, &awsrds.ListTagsForResourceInput{
		ResourceName: strPtr(in.ResourceName),
	})
	if err != nil {
		return nil, fmt.Errorf("provider rds: ListTagsForResource(%s): %w", in.ResourceName, err)
	}
	if res == nil {
		return nil, nil
	}
	return &krds.ListTagsForResourceOutput{TagList: a.tagMap(res.TagList, in.ResourceName)}, nil
}

// --- CommitmentAPI ---------------------------------------------------------

// DescribeReservedDBInstances implements [krds.CommitmentAPI].
//
// FixedPrice, UsagePrice and Duration are copied RAW. The amortization
// (EffectiveHourly = UsagePrice + FixedPrice ÷ term hours) and the
// active/payment-pending filter live in pkg/rds.reservationFromRecord and are
// deliberately not repeated here.
func (a *RDSAPI) DescribeReservedDBInstances(ctx context.Context,
	in *krds.DescribeReservedDBInstancesInput) (*krds.DescribeReservedDBInstancesOutput, error) {

	if in == nil {
		in = &krds.DescribeReservedDBInstancesInput{}
	}
	cctx, cancel := a.call(ctx)
	defer cancel()
	res, err := a.api.DescribeReservedDBInstances(cctx, &awsrds.DescribeReservedDBInstancesInput{
		Marker: strPtr(in.Marker),
	})
	if err != nil {
		return nil, fmt.Errorf("provider rds: DescribeReservedDBInstances: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	out := &krds.DescribeReservedDBInstancesOutput{Marker: str(res.Marker)}
	for _, r := range res.ReservedDBInstances {
		id := str(r.ReservedDBInstanceId)
		// A nil DBInstanceCount becomes 0, and pkg/rds drops a reservation
		// with a non-positive count rather than guessing 1. That is the right
		// call and it is invisible from inside the seam, so name it.
		if r.DBInstanceCount == nil {
			a.notes.add("reserved DB instance %s reported no DBInstanceCount; it is dropped rather "+
				"than counted as one, so it contributes no coverage to this report", orUnnamed(id))
		}
		// A nil State becomes "", which pkg/rds accepts alongside "active".
		if r.State == nil {
			a.notes.add("reserved DB instance %s reported no State; it is treated as billing, which "+
				"is the conservative reading — a retired reservation counted as live can only make a "+
				"saving smaller, never larger", orUnnamed(id))
		}
		// A nil Duration means the upfront cannot be amortized; pkg/rds keeps
		// the usage price alone, which under-states the reservation.
		if r.Duration == nil && r.FixedPrice != nil && *r.FixedPrice > 0 {
			a.notes.add("reserved DB instance %s reported an upfront price but no Duration; the "+
				"upfront cannot be amortized and is dropped, under-stating this reservation's cost "+
				"and therefore under-stating stranding", orUnnamed(id))
		}
		out.ReservedDBInstances = append(out.ReservedDBInstances, krds.ReservedDBInstanceRecord{
			ReservedDBInstanceId: id,
			DBInstanceClass:      str(r.DBInstanceClass),
			DBInstanceCount:      int(i32(r.DBInstanceCount)),
			ProductDescription:   str(r.ProductDescription),
			MultiAZ:              bval(r.MultiAZ),
			OfferingType:         str(r.OfferingType),
			State:                str(r.State),
			FixedPrice:           f64(r.FixedPrice),
			UsagePrice:           f64(r.UsagePrice),
			// Duration is seconds and widens *int32 → int64 losslessly: the
			// longest RDS term is three years, 94,608,000 s.
			Duration:  int64(i32(r.Duration)),
			StartTime: tval(r.StartTime),
		})
	}
	return out, nil
}

// --- ModificationEnvelopeAPI -----------------------------------------------

// DescribeValidDBInstanceModifications implements
// [krds.ModificationEnvelopeAPI]: the live provisioning envelope, read rather
// than hardcoded because AWS's own storage page states two contradictory gp3
// IOPS ceilings.
//
// AWS answers in ranges ([]Range of From/To/Step); ValidStorageOptionRecord
// carries one overall minimum and maximum per dimension, which its own doc
// comment says is the reduction to perform. A storage type whose CEILING
// cannot be read is omitted entirely — see [storageOptionRecord].
func (a *RDSAPI) DescribeValidDBInstanceModifications(ctx context.Context,
	in *krds.DescribeValidDBInstanceModificationsInput) (
	*krds.DescribeValidDBInstanceModificationsOutput, error) {

	if in == nil || strings.TrimSpace(in.DBInstanceIdentifier) == "" {
		return nil, fmt.Errorf("provider rds: DescribeValidDBInstanceModifications needs a DB instance identifier")
	}
	cctx, cancel := a.call(ctx)
	defer cancel()
	res, err := a.api.DescribeValidDBInstanceModifications(cctx,
		&awsrds.DescribeValidDBInstanceModificationsInput{
			DBInstanceIdentifier: strPtr(in.DBInstanceIdentifier),
		})
	if err != nil {
		return nil, fmt.Errorf("provider rds: DescribeValidDBInstanceModifications(%s): %w",
			in.DBInstanceIdentifier, err)
	}
	if res == nil || res.ValidDBInstanceModificationsMessage == nil {
		// An answer with no message is not an empty envelope: returning an
		// output with no options leaves every StorageEnvelope Known=false,
		// which is the refusing default pkg/rds wants.
		a.notes.add("rds:DescribeValidDBInstanceModifications(%s) answered without a modifications "+
			"message; that instance's provisioning envelope stays UNKNOWN and every provisioning "+
			"proposal for it is refused by name", in.DBInstanceIdentifier)
		return &krds.DescribeValidDBInstanceModificationsOutput{}, nil
	}
	out := &krds.DescribeValidDBInstanceModificationsOutput{}
	for _, so := range res.ValidDBInstanceModificationsMessage.Storage {
		if rec, ok := a.storageOptionRecord(so, in.DBInstanceIdentifier); ok {
			out.ValidStorageOptions = append(out.ValidStorageOptions, rec)
		}
	}
	return out, nil
}

// storageOptionRecord reduces one ValidStorageOptions to the record pkg/rds
// carries, and reports whether it may be emitted at all.
//
// The gate is the single most consequential decision in this file.
// pkg/rds/parity.go enforces the ceiling as `env.MaxIOPS > 0 && c.IOPS >
// env.MaxIOPS` — so a StorageEnvelope that is Known with MaxIOPS == 0 has NO
// ceiling, not an unknown one. And pkg/rds sets Known=true for any record
// carrying a storage type. Emitting a record whose ranges AWS did not fill
// would therefore convert "AWS did not tell us the ceiling" into "this
// instance has no ceiling", and a proposal of 80,000 IOPS against an instance
// capped at 16,000 would pass validation and be rejected by AWS at apply time.
//
// So a record is emitted only when AWS named an upper bound for BOTH
// provisionable dimensions. Otherwise the type is left out, Envelope.For
// returns Known=false, and the proposal is refused under the name
// provisioning-envelope-unknown. This over-refuses for storage types where
// nothing is provisionable in the first place (gp2, standard, and io1's
// throughput) — which costs nothing, because pkg/rds only ever looks up gp3.
func (a *RDSAPI) storageOptionRecord(in rdstypes.ValidStorageOptions, id string) (
	krds.ValidStorageOptionRecord, bool) {

	st := strings.ToLower(strings.TrimSpace(str(in.StorageType)))
	if st == "" {
		a.notes.add("rds:DescribeValidDBInstanceModifications(%s) returned a storage option with no "+
			"storage type; it is dropped", id)
		return krds.ValidStorageOptionRecord{}, false
	}
	minIOPS, maxIOPS, iopsCapped, iopsStepped := reduceRanges(in.ProvisionedIops)
	minTP, maxTP, tpCapped, tpStepped := reduceRanges(in.ProvisionedStorageThroughput)
	minSize, maxSize, _, _ := reduceRanges(in.StorageSize)

	if !iopsCapped || !tpCapped {
		a.notes.add("rds:DescribeValidDBInstanceModifications named no provisionable %s ceiling for "+
			"storage type %q on %s; that envelope is reported UNKNOWN rather than known-with-a-zero-"+
			"ceiling, because pkg/rds reads a zero maximum as \"no ceiling to enforce\"",
			missingDimension(iopsCapped, tpCapped), st, id)
		return krds.ValidStorageOptionRecord{}, false
	}
	if iopsStepped || tpStepped {
		a.notes.add("rds:DescribeValidDBInstanceModifications reports storage type %q in steps larger "+
			"than 1; ValidStorageOptionRecord carries only an overall minimum and maximum, so a value "+
			"inside the range but off the step would pass this package's check and be rejected by AWS", st)
	}
	return krds.ValidStorageOptionRecord{
		StorageType:              st,
		MinIOPS:                  minIOPS,
		MaxIOPS:                  maxIOPS,
		MinStorageThroughputMBps: minTP,
		MaxStorageThroughputMBps: maxTP,
		MinAllocatedStorageGiB:   int64(minSize),
		MaxAllocatedStorageGiB:   int64(maxSize),
	}, true
}

// reduceRanges collapses AWS's []Range into the single (min, max) pair
// ValidStorageOptionRecord carries.
//
// A nil From or To is skipped rather than read as 0: "AWS did not name a
// bound" is not "AWS named the bound zero". capped reports whether any range
// named a POSITIVE upper bound, which is the only reading under which the
// resulting maximum can be enforced.
func reduceRanges(rs []rdstypes.Range) (lo, hi int32, capped, stepped bool) {
	haveLo := false
	for _, r := range rs {
		if r.From != nil && (!haveLo || *r.From < lo) {
			lo, haveLo = *r.From, true
		}
		if r.To != nil && *r.To > hi {
			hi, capped = *r.To, true
		}
		if r.Step != nil && *r.Step > 1 {
			stepped = true
		}
	}
	return lo, hi, capped, stepped
}

func missingDimension(iopsCapped, tpCapped bool) string {
	switch {
	case !iopsCapped && !tpCapped:
		return "IOPS or throughput"
	case !iopsCapped:
		return "IOPS"
	default:
		return "throughput"
	}
}

// DescribeEvents implements [krds.ModificationEnvelopeAPI]: the recent
// modification history behind the four-storage-modifications-per-24-hours
// limit. One page per call; the caller owns the loop and the page budget.
func (a *RDSAPI) DescribeEvents(ctx context.Context,
	in *krds.DescribeEventsInput) (*krds.DescribeEventsOutput, error) {

	if in == nil {
		in = &krds.DescribeEventsInput{}
	}
	cctx, cancel := a.call(ctx)
	defer cancel()
	res, err := a.api.DescribeEvents(cctx, &awsrds.DescribeEventsInput{
		SourceIdentifier: strPtr(in.SourceIdentifier),
		SourceType:       rdstypes.SourceType(in.SourceType),
		// A zero time is "the caller did not bound this side", not the year 1;
		// sending it would make AWS reject the whole request.
		StartTime: timePtr(in.StartTime),
		EndTime:   timePtr(in.EndTime),
		Marker:    strPtr(in.Marker),
	})
	if err != nil {
		return nil, fmt.Errorf("provider rds: DescribeEvents(%s): %w", in.SourceIdentifier, err)
	}
	if res == nil {
		return nil, nil
	}
	out := &krds.DescribeEventsOutput{Marker: str(res.Marker)}
	for _, e := range res.Events {
		rec := krds.EventRecord{
			SourceIdentifier: str(e.SourceIdentifier),
			SourceType:       string(e.SourceType),
			Message:          str(e.Message),
			Categories:       nonEmpty(e.EventCategories),
			Date:             tval(e.Date),
		}
		// An undated event keeps its zero time, and Envelope.Cooldown counts
		// only events inside the trailing 24 hours — so an undated storage
		// modification drops out of the count. Under-counting is the error
		// pkg/rds explicitly calls the worse one ("under-counting proposes a
		// change AWS will reject"), so it is named here.
		if e.Date == nil && krds.IsStorageModificationEvent(rec) {
			a.notes.add("rds:DescribeEvents returned a storage-modification event on %s with no date; "+
				"it cannot fall inside the 24-hour window and so does NOT count toward the "+
				"four-modifications limit, which under-counts", orUnnamed(rec.SourceIdentifier))
		}
		out.Events = append(out.Events, rec)
	}
	return out, nil
}

// --- Shared helpers --------------------------------------------------------

// IsAccessDenied reports whether err is AWS refusing for want of an IAM
// permission, as opposed to failing for any other reason.
//
// It exists so cmd/ can act on the difference the IAM table in
// pkg/rds/FINDINGS.md §6.2 draws: cloudwatch:GetMetricData and
// rds:DescribeReservedDBInstances are OPTIONAL, and the documented behaviour
// without them is a degraded report — which cmd/ produces by passing a nil
// seam, not by letting the call fail. See RDS-ADAPTER-FINDINGS.md §5.
//
// Only permission denials are matched. A throttle, a timeout or a malformed
// request must stay an error: swallowing those would turn a transient fault
// into a permanently degraded report that says the credential lacks a
// permission it actually holds.
func IsAccessDenied(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation",
		"AuthorizationError", "AuthFailure", "NotAuthorized", "Forbidden":
		return true
	}
	return false
}

// noteSet collects the facts an adapter learned that its seam's structs have
// no field for. Deduplicated, because one malformed field usually repeats
// across a page, and sorted, because a report that reorders itself between
// runs cannot be diffed.
type noteSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (n *noteSet) add(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[string]bool{}
	}
	n.seen[s] = true
}

func (n *noteSet) list() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(n.seen))
	for s := range n.seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func orUnnamed(s string) string {
	if strings.TrimSpace(s) == "" {
		return "an unnamed resource"
	}
	return s
}

// nonEmpty copies a string slice, dropping blanks. AWS occasionally lists an
// empty member identifier; carrying it forward would invent a replica, a
// cluster member or an event category named "".
func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func f64(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func bval(v *bool) bool {
	if v == nil {
		return false
	}
	return *v
}

func tval(v *time.Time) time.Time {
	if v == nil {
		return time.Time{}
	}
	return *v
}

// strPtr returns nil for the empty string, so an unset seam field stays unset
// on the wire instead of becoming an explicit empty filter.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func i32Ptr(v int32) *int32 {
	if v == 0 {
		return nil
	}
	return &v
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
