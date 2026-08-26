package provider

// Every test here runs against a FAKE SDK client. Nothing in this file reads a
// credential, opens ~/.aws, reads an AWS_* variable or opens a socket — the
// same guarantee pkg/rds gives, for the same reason: an adapter you can only
// test against a real account is an adapter nobody tests.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	awsrds "github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/agenticode/kilter/pkg/pricing/commit"
	krds "github.com/agenticode/kilter/pkg/rds"
)

// ---- pointer helpers (sp/ip already exist in provider_test.go) ----

func bptr(b bool) *bool           { return &b }
func fptr(f float64) *float64     { return &f }
func tptr(t time.Time) *time.Time { return &t }

// ---- the fake rds: client ----

type fakeRDS struct {
	instances []rdstypes.DBInstance
	clusters  []rdstypes.DBCluster
	tags      map[string][]rdstypes.Tag
	reserved  []rdstypes.ReservedDBInstance
	options   map[string]*rdstypes.ValidDBInstanceModificationsMessage
	events    map[string][]rdstypes.Event
	// pageSize splits every paginated response; 0 means one page.
	pageSize int

	err error
	// deadlines records whether each call arrived with a deadline set.
	mu        sync.Mutex
	deadlines []bool
	calls     []string
	lastQuery any
}

func (f *fakeRDS) record(op string, ctx context.Context, in any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := ctx.Deadline()
	f.deadlines = append(f.deadlines, ok)
	f.calls = append(f.calls, op)
	f.lastQuery = in
}

// page slices a fake collection the way an AWS Marker does.
func (f *fakeRDS) page(total int, marker *string) (start, end int, next *string) {
	start = 0
	if marker != nil {
		start, _ = strconv.Atoi(*marker)
	}
	if start > total {
		start = total
	}
	end = total
	if f.pageSize > 0 && start+f.pageSize < total {
		end = start + f.pageSize
		tok := strconv.Itoa(end)
		next = &tok
	}
	return start, end, next
}

func (f *fakeRDS) DescribeDBInstances(ctx context.Context, in *awsrds.DescribeDBInstancesInput,
	_ ...func(*awsrds.Options)) (*awsrds.DescribeDBInstancesOutput, error) {
	f.record("DescribeDBInstances", ctx, in)
	if f.err != nil {
		return nil, f.err
	}
	s, e, next := f.page(len(f.instances), in.Marker)
	return &awsrds.DescribeDBInstancesOutput{DBInstances: f.instances[s:e], Marker: next}, nil
}

func (f *fakeRDS) DescribeDBClusters(ctx context.Context, in *awsrds.DescribeDBClustersInput,
	_ ...func(*awsrds.Options)) (*awsrds.DescribeDBClustersOutput, error) {
	f.record("DescribeDBClusters", ctx, in)
	if f.err != nil {
		return nil, f.err
	}
	s, e, next := f.page(len(f.clusters), in.Marker)
	return &awsrds.DescribeDBClustersOutput{DBClusters: f.clusters[s:e], Marker: next}, nil
}

func (f *fakeRDS) ListTagsForResource(ctx context.Context, in *awsrds.ListTagsForResourceInput,
	_ ...func(*awsrds.Options)) (*awsrds.ListTagsForResourceOutput, error) {
	f.record("ListTagsForResource", ctx, in)
	if f.err != nil {
		return nil, f.err
	}
	return &awsrds.ListTagsForResourceOutput{TagList: f.tags[str(in.ResourceName)]}, nil
}

func (f *fakeRDS) DescribeReservedDBInstances(ctx context.Context, in *awsrds.DescribeReservedDBInstancesInput,
	_ ...func(*awsrds.Options)) (*awsrds.DescribeReservedDBInstancesOutput, error) {
	f.record("DescribeReservedDBInstances", ctx, in)
	if f.err != nil {
		return nil, f.err
	}
	s, e, next := f.page(len(f.reserved), in.Marker)
	return &awsrds.DescribeReservedDBInstancesOutput{ReservedDBInstances: f.reserved[s:e], Marker: next}, nil
}

func (f *fakeRDS) DescribeValidDBInstanceModifications(ctx context.Context,
	in *awsrds.DescribeValidDBInstanceModificationsInput,
	_ ...func(*awsrds.Options)) (*awsrds.DescribeValidDBInstanceModificationsOutput, error) {
	f.record("DescribeValidDBInstanceModifications", ctx, in)
	if f.err != nil {
		return nil, f.err
	}
	return &awsrds.DescribeValidDBInstanceModificationsOutput{
		ValidDBInstanceModificationsMessage: f.options[str(in.DBInstanceIdentifier)]}, nil
}

func (f *fakeRDS) DescribeEvents(ctx context.Context, in *awsrds.DescribeEventsInput,
	_ ...func(*awsrds.Options)) (*awsrds.DescribeEventsOutput, error) {
	f.record("DescribeEvents", ctx, in)
	if f.err != nil {
		return nil, f.err
	}
	all := f.events[str(in.SourceIdentifier)]
	s, e, next := f.page(len(all), in.Marker)
	return &awsrds.DescribeEventsOutput{Events: all[s:e], Marker: next}, nil
}

// ---- the seams are satisfied, and the write surface is empty ----

func TestRDSAdapterSatisfiesEverySeamPkgRDSDeclares(t *testing.T) {
	a := newRDSAPI(&fakeRDS{}, "us-east-1")
	var (
		_ krds.InventoryAPI            = a
		_ krds.CommitmentAPI           = a
		_ krds.ModificationEnvelopeAPI = a
	)
	if a.Region() != "us-east-1" {
		t.Fatalf("Region() = %q", a.Region())
	}
	// The collector is the real consumer; it must accept the adapter in all
	// three positions at once.
	cw := newCloudWatchAPI(&fakeCW{}, "us-east-1")
	cfg := krds.DefaultCollectorConfig(krds.Window{Start: refNow.Add(-24 * time.Hour), End: refNow})
	if _, err := krds.NewCollector(a, cw, a, cfg); err != nil {
		t.Fatalf("NewCollector over the live adapter: %v", err)
	}
}

// TestNoMutatingSDKSurface is this package's copy of pkg/rds's
// TestNoMutatingAPISurface: the SDK interfaces are the whole AWS surface the
// RDS path can reach, and a reviewer must be able to see that it is read-only
// without reading the bodies.
func TestNoMutatingSDKSurface(t *testing.T) {
	mutating := []string{"create", "modify", "delete", "reboot", "start", "stop",
		"promote", "restore", "failover", "apply", "purchase", "add", "remove", "put"}
	for _, iface := range []reflect.Type{
		reflect.TypeOf((*rdsSDK)(nil)).Elem(),
		reflect.TypeOf((*cloudwatchSDK)(nil)).Elem(),
	} {
		for i := 0; i < iface.NumMethod(); i++ {
			name := strings.ToLower(iface.Method(i).Name)
			for _, verb := range mutating {
				if strings.HasPrefix(name, verb) {
					t.Errorf("%s declares %s, which is not a read operation",
						iface.Name(), iface.Method(i).Name)
				}
			}
		}
	}
}

// ---- the field copy ----

var refNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func fullInstance() rdstypes.DBInstance {
	return rdstypes.DBInstance{
		DBInstanceIdentifier:                  sp("prod-orders"),
		DBInstanceArn:                         sp("arn:aws:rds:us-east-1:123456789012:db:prod-orders"),
		DBInstanceClass:                       sp("db.r6i.xlarge"),
		DBInstanceStatus:                      sp("available"),
		Engine:                                sp("postgres"),
		EngineVersion:                         sp("15.5"),
		LicenseModel:                          sp("postgresql-license"),
		MultiAZ:                               bptr(true),
		DBClusterIdentifier:                   sp(""),
		AvailabilityZone:                      sp("us-east-1a"),
		ReadReplicaSourceDBInstanceIdentifier: sp(""),
		ReadReplicaDBInstanceIdentifiers:      []string{"prod-orders-ro", "", "prod-orders-ro2"},
		AllocatedStorage:                      ip(500),
		MaxAllocatedStorage:                   ip(1000),
		StorageType:                           sp("gp3"),
		Iops:                                  ip(12000),
		StorageThroughput:                     ip(500),
		InstanceCreateTime:                    tptr(refNow.Add(-90 * 24 * time.Hour)),
		TagList: []rdstypes.Tag{
			{Key: sp("env"), Value: sp("prod")},
			{Key: sp(krds.TagKilterMode), Value: sp("report")},
		},
	}
}

func TestDBInstanceRecordIsAFieldForFieldCopy(t *testing.T) {
	a := newRDSAPI(&fakeRDS{instances: []rdstypes.DBInstance{fullInstance()}}, "us-east-1")
	out, err := a.DescribeDBInstances(context.Background(), &krds.DescribeDBInstancesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.DBInstances) != 1 {
		t.Fatalf("want 1 record, got %d", len(out.DBInstances))
	}
	got := out.DBInstances[0]
	want := krds.DBInstanceRecord{
		DBInstanceIdentifier: "prod-orders",
		DBInstanceArn:        "arn:aws:rds:us-east-1:123456789012:db:prod-orders",
		DBInstanceClass:      "db.r6i.xlarge",
		DBInstanceStatus:     "available",
		Engine:               "postgres",
		EngineVersion:        "15.5",
		LicenseModel:         "postgresql-license",
		MultiAZ:              true,
		AvailabilityZone:     "us-east-1a",
		// The blank entry in the SDK list is dropped rather than carried
		// forward as a replica named "".
		ReadReplicaDBInstanceIdentifiers: []string{"prod-orders-ro", "prod-orders-ro2"},
		AllocatedStorage:                 500,
		MaxAllocatedStorage:              1000,
		StorageType:                      "gp3",
		Iops:                             12000,
		StorageThroughput:                500,
		InstanceCreateTime:               refNow.Add(-90 * 24 * time.Hour),
		TagList:                          map[string]string{"env": "prod", krds.TagKilterMode: "report"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("field copy differs:\n got %+v\nwant %+v", got, want)
	}
	if notes := a.Notes(); len(notes) != 0 {
		t.Fatalf("a well-formed record must produce no notes: %v", notes)
	}
}

// TestEveryNilableInstanceFieldHasADecision walks the SDK struct with
// reflection so a new nilable field cannot be added upstream and quietly
// default to zero without anyone deciding what its absence means.
func TestEveryNilableInstanceFieldHasADecision(t *testing.T) {
	// The fields this adapter reads. Any of them may arrive nil.
	read := []string{
		"DBInstanceIdentifier", "DBInstanceArn", "DBInstanceClass", "DBInstanceStatus",
		"Engine", "EngineVersion", "LicenseModel", "MultiAZ", "DBClusterIdentifier",
		"AvailabilityZone", "ReadReplicaSourceDBInstanceIdentifier",
		"ReadReplicaDBInstanceIdentifiers", "AllocatedStorage", "MaxAllocatedStorage",
		"StorageType", "Iops", "StorageThroughput", "InstanceCreateTime", "TagList",
	}
	typ := reflect.TypeOf(rdstypes.DBInstance{})
	for _, name := range read {
		if _, ok := typ.FieldByName(name); !ok {
			t.Fatalf("the SDK no longer has DBInstance.%s; the field copy is stale", name)
		}
	}

	// An entirely unset instance: every pointer nil, every slice nil.
	a := newRDSAPI(&fakeRDS{instances: []rdstypes.DBInstance{{}}}, "us-east-1")
	out, err := a.DescribeDBInstances(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := out.DBInstances[0]
	if !reflect.DeepEqual(got, krds.DBInstanceRecord{}) {
		t.Fatalf("an all-nil SDK instance must produce the zero record, got %+v", got)
	}
	// …and it must SAY so, because pkg/rds drops an unaddressable record
	// silently.
	if !hasNote(a.Notes(), "neither DBInstanceArn nor DBInstanceIdentifier") {
		t.Fatalf("an unaddressable instance must be visible in Notes(): %v", a.Notes())
	}
}

// TestNilMultiAZUnderStatesRatherThanOverStates pins the one nilable field
// whose destination cannot carry "unknown" and whose fallback moves money.
func TestNilMultiAZUnderStatesRatherThanOverStates(t *testing.T) {
	inst := fullInstance()
	inst.MultiAZ = nil
	a := newRDSAPI(&fakeRDS{instances: []rdstypes.DBInstance{inst}}, "us-east-1")
	out, _ := a.DescribeDBInstances(context.Background(), &krds.DescribeDBInstancesInput{})
	if out.DBInstances[0].MultiAZ {
		t.Fatal("a nil MultiAZ must not become true")
	}
	// Single-AZ is a ×1 multiplier where Multi-AZ is ×2: the copy under-states
	// the instance line, and an under-stated line can only under-state a
	// saving. Prove pkg/rds reads it that way rather than refusing.
	d := krds.DBInstance{Identifier: "x", MultiAZ: out.DBInstances[0].MultiAZ}
	dep, ok := d.Deployment()
	if !ok || dep != commit.RDSSingleAZ {
		t.Fatalf("Deployment() = %v, %v; want single-AZ", dep, ok)
	}
}

func TestNilTagKeyIsDroppedAndNoted(t *testing.T) {
	inst := fullInstance()
	inst.TagList = []rdstypes.Tag{
		{Key: nil, Value: sp("off")},
		{Key: sp("keep"), Value: nil},
	}
	a := newRDSAPI(&fakeRDS{instances: []rdstypes.DBInstance{inst}}, "us-east-1")
	out, _ := a.DescribeDBInstances(context.Background(), &krds.DescribeDBInstancesInput{})
	tags := out.DBInstances[0].TagList
	if _, bad := tags[""]; bad {
		t.Fatal("a tag with no key must not become a tag named \"\"")
	}
	// A nil Value is an empty-valued tag, which is legal and is NOT "off".
	if v, ok := tags["keep"]; !ok || v != "" {
		t.Fatalf("tags = %v; want keep=\"\"", tags)
	}
	if !hasNote(a.Notes(), "tag with no key") {
		t.Fatalf("a dropped tag must be visible: %v", a.Notes())
	}
}

func TestListTagsForResourceRequiresAnARN(t *testing.T) {
	a := newRDSAPI(&fakeRDS{}, "us-east-1")
	if _, err := a.ListTagsForResource(context.Background(), &krds.ListTagsForResourceInput{}); err == nil {
		t.Fatal("an empty resource name must be refused client-side")
	}
	if _, err := a.ListTagsForResource(context.Background(), nil); err == nil {
		t.Fatal("a nil input must be refused client-side")
	}
}

func TestClusterRecordCopyAndNilServerlessConfig(t *testing.T) {
	f := &fakeRDS{clusters: []rdstypes.DBCluster{
		{
			DBClusterIdentifier: sp("aurora-1"),
			DBClusterArn:        sp("arn:aws:rds:us-east-1:1:cluster:aurora-1"),
			Engine:              sp("aurora-postgresql"),
			EngineMode:          sp("provisioned"),
			DBClusterMembers: []rdstypes.DBClusterMember{
				{DBInstanceIdentifier: sp("aurora-1-a")},
				{DBInstanceIdentifier: nil},
			},
			ServerlessV2ScalingConfiguration: &rdstypes.ServerlessV2ScalingConfigurationInfo{
				MinCapacity: fptr(0.5), MaxCapacity: fptr(16),
			},
		},
		// A provisioned Multi-AZ DB cluster: no serverless configuration at all.
		{DBClusterIdentifier: sp("mysql-maz"), Engine: sp("mysql")},
	}}
	a := newRDSAPI(f, "us-east-1")
	out, err := a.DescribeDBClusters(context.Background(), &krds.DescribeDBClustersInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := out.DBClusters[0]
	want := krds.DBClusterRecord{
		DBClusterIdentifier:     "aurora-1",
		DBClusterArn:            "arn:aws:rds:us-east-1:1:cluster:aurora-1",
		Engine:                  "aurora-postgresql",
		EngineMode:              "provisioned",
		DBClusterMembers:        []string{"aurora-1-a"},
		ServerlessV2MinCapacity: 0.5,
		ServerlessV2MaxCapacity: 16,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cluster copy:\n got %+v\nwant %+v", got, want)
	}
	// A cluster with no ServerlessV2 configuration leaves the ACU bounds at 0.
	// pkg/rds never does arithmetic with them, so 0 costs nothing — but it
	// must not become a nil-pointer dereference on the way through.
	second := out.DBClusters[1]
	if second.ServerlessV2MinCapacity != 0 || second.ServerlessV2MaxCapacity != 0 {
		t.Fatalf("a cluster with no serverless config must leave the ACU bounds at 0: %+v", second)
	}
}

// ---- pagination, driven by the real collector ----

func TestPaginationIsPropagatedThroughEverySeam(t *testing.T) {
	var instances []rdstypes.DBInstance
	for i := 0; i < 7; i++ {
		in := fullInstance()
		id := fmt.Sprintf("db-%d", i)
		in.DBInstanceIdentifier = sp(id)
		in.DBInstanceArn = sp("arn:aws:rds:us-east-1:1:db:" + id)
		instances = append(instances, in)
	}
	var reserved []rdstypes.ReservedDBInstance
	for i := 0; i < 5; i++ {
		reserved = append(reserved, rdstypes.ReservedDBInstance{
			ReservedDBInstanceId: sp(fmt.Sprintf("ri-%d", i)),
			DBInstanceClass:      sp("db.r6i.xlarge"),
			DBInstanceCount:      ip(1),
			ProductDescription:   sp("postgresql"),
			State:                sp("active"),
			Duration:             ip(31536000),
			UsagePrice:           fptr(0.1),
			FixedPrice:           fptr(876),
			StartTime:            tptr(refNow.Add(-30 * 24 * time.Hour)),
		})
	}
	f := &fakeRDS{instances: instances, reserved: reserved, pageSize: 3}
	a := newRDSAPI(f, "us-east-1")

	cfg := krds.DefaultCollectorConfig(krds.Window{Start: refNow.Add(-24 * time.Hour), End: refNow})
	cfg.Scope, cfg.Region = "acct/us-east-1", "us-east-1"
	c, err := krds.NewCollector(a, nil, a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Targets) != 7 {
		t.Fatalf("pagination lost instances: got %d of 7", len(snap.Targets))
	}
	if len(snap.Reservations) != 5 {
		t.Fatalf("pagination lost reservations: got %d of 5", len(snap.Reservations))
	}
	// Three pages of instances (3+3+1) and two of reservations (3+2).
	if n := countCalls(f, "DescribeDBInstances"); n != 3 {
		t.Fatalf("want 3 instance pages, got %d", n)
	}
	if n := countCalls(f, "DescribeReservedDBInstances"); n != 2 {
		t.Fatalf("want 2 reservation pages, got %d", n)
	}
}

// TestSwallowingAMarkerWouldTruncateSilently is the negative of the test
// above: it shows what a dropped Marker costs, so the propagation is not
// mistaken for incidental.
func TestSwallowingAMarkerWouldTruncateSilently(t *testing.T) {
	f := &fakeRDS{pageSize: 3}
	for i := 0; i < 7; i++ {
		in := fullInstance()
		in.DBInstanceIdentifier = sp(fmt.Sprintf("db-%d", i))
		in.DBInstanceArn = sp(fmt.Sprintf("arn:aws:rds:us-east-1:1:db:db-%d", i))
		f.instances = append(f.instances, in)
	}
	a := newRDSAPI(f, "us-east-1")
	first, err := a.DescribeDBInstances(context.Background(), &krds.DescribeDBInstancesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Marker == "" {
		t.Fatal("AWS said there is more; the adapter must say so too, or the inventory truncates " +
			"into a report that reads as complete")
	}
	second, err := a.DescribeDBInstances(context.Background(),
		&krds.DescribeDBInstancesInput{Marker: first.Marker})
	if err != nil {
		t.Fatal(err)
	}
	if second.DBInstances[0].DBInstanceIdentifier != "db-3" {
		t.Fatalf("the inbound marker was not honoured: page 2 starts at %q",
			second.DBInstances[0].DBInstanceIdentifier)
	}
}

func TestMaxRecordsIsOnlySentWhenAsked(t *testing.T) {
	f := &fakeRDS{}
	a := newRDSAPI(f, "us-east-1")
	if _, err := a.DescribeDBInstances(context.Background(), &krds.DescribeDBInstancesInput{}); err != nil {
		t.Fatal(err)
	}
	if in := f.lastQuery.(*awsrds.DescribeDBInstancesInput); in.MaxRecords != nil {
		t.Fatalf("MaxRecords sent unasked as %d; AWS rejects anything outside 20–100", *in.MaxRecords)
	}
	if _, err := a.DescribeDBInstances(context.Background(),
		&krds.DescribeDBInstancesInput{MaxRecords: 100}); err != nil {
		t.Fatal(err)
	}
	if in := f.lastQuery.(*awsrds.DescribeDBInstancesInput); in.MaxRecords == nil || *in.MaxRecords != 100 {
		t.Fatalf("MaxRecords not propagated: %v", in.MaxRecords)
	}
}

// ---- reservations: raw in, amortized by pkg/rds, never twice ----

func TestReservationFieldsAreCopiedRawAndAmortizedOnlyByPkgRDS(t *testing.T) {
	f := &fakeRDS{reserved: []rdstypes.ReservedDBInstance{{
		ReservedDBInstanceId: sp("ri-1"),
		DBInstanceClass:      sp("db.r6i.xlarge"),
		DBInstanceCount:      ip(2),
		ProductDescription:   sp("postgresql"),
		MultiAZ:              bptr(true),
		OfferingType:         sp("Partial Upfront"),
		State:                sp("active"),
		FixedPrice:           fptr(8760),
		UsagePrice:           fptr(0.05),
		Duration:             ip(31536000), // 1 year in seconds → 8,760 hours
		StartTime:            tptr(refNow),
	}}}
	a := newRDSAPI(f, "us-east-1")
	out, err := a.DescribeReservedDBInstances(context.Background(),
		&krds.DescribeReservedDBInstancesInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := out.ReservedDBInstances[0]
	want := krds.ReservedDBInstanceRecord{
		ReservedDBInstanceId: "ri-1", DBInstanceClass: "db.r6i.xlarge", DBInstanceCount: 2,
		ProductDescription: "postgresql", MultiAZ: true, OfferingType: "Partial Upfront",
		State: "active", FixedPrice: 8760, UsagePrice: 0.05, Duration: 31536000, StartTime: refNow,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reservation copy:\n got %+v\nwant %+v", got, want)
	}

	// Now prove pkg/rds does the amortization and this adapter did not:
	// 8760 / 8760h = 1.00/h, plus the 0.05 usage price.
	cfg := krds.DefaultCollectorConfig(krds.Window{Start: refNow.Add(-24 * time.Hour), End: refNow})
	cfg.Region = "us-east-1"
	c, _ := krds.NewCollector(a, nil, a, cfg)
	snap, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Reservations) != 1 {
		t.Fatalf("want 1 amortized reservation, got %d", len(snap.Reservations))
	}
	r := snap.Reservations[0]
	if diff := r.EffectiveHourlyUSD - 1.05; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("EffectiveHourlyUSD = %v, want 1.05 (amortized exactly once)", r.EffectiveHourlyUSD)
	}
	// Deployment topology is likewise pkg/rds's, derived from the raw MultiAZ.
	if r.Deployment != commit.RDSMultiAZInstance {
		t.Fatalf("Deployment = %q; the adapter must not re-derive topology", r.Deployment)
	}
}

func TestReservationNilFieldsAreVisible(t *testing.T) {
	f := &fakeRDS{reserved: []rdstypes.ReservedDBInstance{
		{ReservedDBInstanceId: sp("ri-nocount"), DBInstanceClass: sp("db.t3.small"), State: sp("active")},
		{ReservedDBInstanceId: sp("ri-nostate"), DBInstanceClass: sp("db.t3.small"), DBInstanceCount: ip(1)},
		{ReservedDBInstanceId: sp("ri-nodur"), DBInstanceClass: sp("db.t3.small"),
			DBInstanceCount: ip(1), State: sp("active"), FixedPrice: fptr(100)},
	}}
	a := newRDSAPI(f, "us-east-1")
	if _, err := a.DescribeReservedDBInstances(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"reported no DBInstanceCount",
		"reported no State",
		"upfront price but no Duration",
	} {
		if !hasNote(a.Notes(), want) {
			t.Errorf("missing note %q in %v", want, a.Notes())
		}
	}
}

// ---- the provisioning envelope: the decision that moves a verdict ----

func TestEnvelopeRangesAreReducedToOverallMinAndMax(t *testing.T) {
	f := &fakeRDS{options: map[string]*rdstypes.ValidDBInstanceModificationsMessage{
		"db-1": {Storage: []rdstypes.ValidStorageOptions{{
			StorageType: sp("GP3"),
			ProvisionedIops: []rdstypes.Range{
				{From: ip(3000), To: ip(12000)},
				{From: ip(12001), To: ip(64000)},
			},
			ProvisionedStorageThroughput: []rdstypes.Range{
				{From: ip(125), To: ip(500)},
				{From: ip(501), To: ip(4000)},
			},
			StorageSize: []rdstypes.Range{{From: ip(20), To: ip(65536)}},
		}}},
	}}
	a := newRDSAPI(f, "us-east-1")
	out, err := a.DescribeValidDBInstanceModifications(context.Background(),
		&krds.DescribeValidDBInstanceModificationsInput{DBInstanceIdentifier: "db-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := krds.ValidStorageOptionRecord{
		StorageType: "gp3", MinIOPS: 3000, MaxIOPS: 64000,
		MinStorageThroughputMBps: 125, MaxStorageThroughputMBps: 4000,
		MinAllocatedStorageGiB: 20, MaxAllocatedStorageGiB: 65536,
	}
	if len(out.ValidStorageOptions) != 1 || !reflect.DeepEqual(out.ValidStorageOptions[0], want) {
		t.Fatalf("range reduction:\n got %+v\nwant %+v", out.ValidStorageOptions, want)
	}

	// End to end: the envelope becomes Known and enforces its ceiling.
	env := collectEnvelope(t, a, "db-1")
	gp3 := env.Get("db-1").For("gp3")
	if !gp3.Known || gp3.MaxIOPS != 64000 {
		t.Fatalf("collected envelope = %+v", gp3)
	}
}

// TestEnvelopeWithNoReadableCeilingStaysUnknown is the load-bearing test in
// this file.
//
// pkg/rds enforces the ceiling as `env.MaxIOPS > 0 && c.IOPS > env.MaxIOPS`,
// so a Known envelope with a zero maximum has NO ceiling rather than an
// unknown one. If this adapter emitted a record whose ranges AWS never filled,
// "AWS did not tell us the ceiling" would silently become "this instance has
// no ceiling" and an 80,000-IOPS proposal would sail past validation on an
// instance capped at 16,000.
func TestEnvelopeWithNoReadableCeilingStaysUnknown(t *testing.T) {
	cases := []struct {
		name string
		opt  rdstypes.ValidStorageOptions
	}{
		{"no ranges at all", rdstypes.ValidStorageOptions{StorageType: sp("gp3")}},
		{"iops range with a nil upper bound", rdstypes.ValidStorageOptions{
			StorageType:                  sp("gp3"),
			ProvisionedIops:              []rdstypes.Range{{From: ip(3000), To: nil}},
			ProvisionedStorageThroughput: []rdstypes.Range{{From: ip(125), To: ip(4000)}},
		}},
		{"throughput ranges absent", rdstypes.ValidStorageOptions{
			StorageType:     sp("gp3"),
			ProvisionedIops: []rdstypes.Range{{From: ip(3000), To: ip(64000)}},
		}},
		{"a zero upper bound is not a ceiling", rdstypes.ValidStorageOptions{
			StorageType:                  sp("gp3"),
			ProvisionedIops:              []rdstypes.Range{{From: ip(0), To: ip(0)}},
			ProvisionedStorageThroughput: []rdstypes.Range{{From: ip(0), To: ip(0)}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRDS{options: map[string]*rdstypes.ValidDBInstanceModificationsMessage{
				"db-1": {Storage: []rdstypes.ValidStorageOptions{tc.opt}},
			}}
			a := newRDSAPI(f, "us-east-1")
			out, err := a.DescribeValidDBInstanceModifications(context.Background(),
				&krds.DescribeValidDBInstanceModificationsInput{DBInstanceIdentifier: "db-1"})
			if err != nil {
				t.Fatal(err)
			}
			if len(out.ValidStorageOptions) != 0 {
				t.Fatalf("an unreadable ceiling must not become a known envelope: %+v",
					out.ValidStorageOptions)
			}
			if env := collectEnvelope(t, a, "db-1"); env.Get("db-1").For("gp3").Known {
				t.Fatal("StorageEnvelope.Known must stay false, so the proposal is refused by name")
			}
			if !hasNote(a.Notes(), "reported UNKNOWN rather than known-with-a-zero-ceiling") {
				t.Fatalf("the omission must be visible: %v", a.Notes())
			}
		})
	}
}

func TestEnvelopeAbsentMessageIsUnknownNotEmpty(t *testing.T) {
	a := newRDSAPI(&fakeRDS{options: map[string]*rdstypes.ValidDBInstanceModificationsMessage{}}, "us-east-1")
	out, err := a.DescribeValidDBInstanceModifications(context.Background(),
		&krds.DescribeValidDBInstanceModificationsInput{DBInstanceIdentifier: "db-1"})
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || len(out.ValidStorageOptions) != 0 {
		t.Fatalf("want an empty output, got %+v", out)
	}
	if !hasNote(a.Notes(), "answered without a modifications message") {
		t.Fatalf("notes = %v", a.Notes())
	}
	if _, err := a.DescribeValidDBInstanceModifications(context.Background(),
		&krds.DescribeValidDBInstanceModificationsInput{}); err == nil {
		t.Fatal("an empty identifier must be refused client-side")
	}
}

func TestEnvelopeStepIsNotedBecauseTheRecordCannotCarryIt(t *testing.T) {
	f := &fakeRDS{options: map[string]*rdstypes.ValidDBInstanceModificationsMessage{
		"db-1": {Storage: []rdstypes.ValidStorageOptions{{
			StorageType:                  sp("gp3"),
			ProvisionedIops:              []rdstypes.Range{{From: ip(3000), To: ip(64000), Step: ip(1000)}},
			ProvisionedStorageThroughput: []rdstypes.Range{{From: ip(125), To: ip(4000)}},
		}}},
	}}
	a := newRDSAPI(f, "us-east-1")
	if _, err := a.DescribeValidDBInstanceModifications(context.Background(),
		&krds.DescribeValidDBInstanceModificationsInput{DBInstanceIdentifier: "db-1"}); err != nil {
		t.Fatal(err)
	}
	if !hasNote(a.Notes(), "in steps larger than 1") {
		t.Fatalf("a step the record cannot carry must be visible: %v", a.Notes())
	}
}

// ---- events ----

func TestEventCopyAndUndatedModificationIsNoted(t *testing.T) {
	f := &fakeRDS{events: map[string][]rdstypes.Event{
		"db-1": {
			{
				SourceIdentifier: sp("db-1"),
				SourceType:       rdstypes.SourceTypeDbInstance,
				Message:          sp("Applying modification to allocated storage"),
				EventCategories:  []string{"configuration change", ""},
				Date:             tptr(refNow.Add(-2 * time.Hour)),
			},
			{
				SourceIdentifier: sp("db-1"),
				Message:          sp("Finished applying modification to Provisioned IOPS"),
				EventCategories:  []string{"configuration change"},
				Date:             nil,
			},
		},
	}}
	a := newRDSAPI(f, "us-east-1")
	out, err := a.DescribeEvents(context.Background(), &krds.DescribeEventsInput{
		SourceIdentifier: "db-1", SourceType: krds.EventSourceDBInstance,
		StartTime: refNow.Add(-24 * time.Hour), EndTime: refNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(out.Events))
	}
	first := out.Events[0]
	if first.SourceType != string(rdstypes.SourceTypeDbInstance) {
		t.Fatalf("SourceType = %q", first.SourceType)
	}
	if !reflect.DeepEqual(first.Categories, []string{"configuration change"}) {
		t.Fatalf("a blank category must be dropped: %v", first.Categories)
	}
	if !out.Events[1].Date.IsZero() {
		t.Fatal("an undated event must keep a zero date rather than being given one")
	}
	if !hasNote(a.Notes(), "with no date") {
		t.Fatalf("an undated storage modification must be visible: %v", a.Notes())
	}

	// The zero-time input bounds must not be sent as year 1.
	if _, err := a.DescribeEvents(context.Background(), &krds.DescribeEventsInput{SourceIdentifier: "db-1"}); err != nil {
		t.Fatal(err)
	}
	in := f.lastQuery.(*awsrds.DescribeEventsInput)
	if in.StartTime != nil || in.EndTime != nil {
		t.Fatalf("a zero window must be sent as unset, got %v..%v", in.StartTime, in.EndTime)
	}
}

func TestEventPaginationFeedsTheCooldownVerdict(t *testing.T) {
	var evs []rdstypes.Event
	for i := 0; i < 4; i++ {
		evs = append(evs, rdstypes.Event{
			SourceIdentifier: sp("db-1"),
			SourceType:       rdstypes.SourceTypeDbInstance,
			Message:          sp("Applying modification to allocated storage"),
			EventCategories:  []string{"configuration change"},
			Date:             tptr(refNow.Add(-time.Duration(i+1) * time.Hour)),
		})
	}
	a := newRDSAPI(&fakeRDS{events: map[string][]rdstypes.Event{"db-1": evs}, pageSize: 2}, "us-east-1")
	env := collectEnvelope(t, a, "db-1")
	v := env.Get("db-1").Cooldown(refNow)
	if !v.Known {
		t.Fatal("the history was read; the cooldown must be Known")
	}
	if v.Recent != 4 || !v.Blocked {
		t.Fatalf("pagination lost modifications: %+v", v)
	}
}

// ---- timeouts, cancellation and error propagation ----

func TestEveryCallCarriesADeadline(t *testing.T) {
	f := &fakeRDS{
		instances: []rdstypes.DBInstance{fullInstance()},
		clusters:  []rdstypes.DBCluster{{DBClusterIdentifier: sp("c1")}},
		reserved:  []rdstypes.ReservedDBInstance{{ReservedDBInstanceId: sp("r1"), DBInstanceCount: ip(1), DBInstanceClass: sp("db.t3.small")}},
		options:   map[string]*rdstypes.ValidDBInstanceModificationsMessage{"db-1": {}},
		events:    map[string][]rdstypes.Event{"db-1": nil},
		tags:      map[string][]rdstypes.Tag{"arn": {{Key: sp("k"), Value: sp("v")}}},
	}
	a := newRDSAPI(f, "us-east-1")
	ctx := context.Background()
	a.DescribeDBInstances(ctx, &krds.DescribeDBInstancesInput{})
	a.DescribeDBClusters(ctx, &krds.DescribeDBClustersInput{})
	a.ListTagsForResource(ctx, &krds.ListTagsForResourceInput{ResourceName: "arn"})
	a.DescribeReservedDBInstances(ctx, &krds.DescribeReservedDBInstancesInput{})
	a.DescribeValidDBInstanceModifications(ctx, &krds.DescribeValidDBInstanceModificationsInput{DBInstanceIdentifier: "db-1"})
	a.DescribeEvents(ctx, &krds.DescribeEventsInput{SourceIdentifier: "db-1"})

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deadlines) != 6 {
		t.Fatalf("want 6 calls, got %d (%v)", len(f.deadlines), f.calls)
	}
	for i, ok := range f.deadlines {
		if !ok {
			t.Errorf("%s was issued with no deadline; a hung call would hold the collection open",
				f.calls[i])
		}
	}
}

func TestSetCallTimeoutBoundsTheCall(t *testing.T) {
	blocked := &blockingRDS{fakeRDS: &fakeRDS{}}
	a := newRDSAPI(blocked, "us-east-1")
	a.SetCallTimeout(20 * time.Millisecond)
	start := time.Now()
	_, err := a.DescribeDBInstances(context.Background(), &krds.DescribeDBInstancesInput{})
	if err == nil {
		t.Fatal("a call that outlives its timeout must fail")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the timeout did not bound the call: %s", elapsed)
	}
	// A non-positive value restores the default rather than disabling the bound.
	a.SetCallTimeout(0)
	if a.timeout != DefaultRDSCallTimeout {
		t.Fatalf("timeout = %s, want the default", a.timeout)
	}
}

func TestParentCancellationStillPropagates(t *testing.T) {
	blocked := &blockingRDS{fakeRDS: &fakeRDS{}}
	a := newRDSAPI(blocked, "us-east-1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.DescribeDBInstances(ctx, &krds.DescribeDBInstancesInput{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want a cancellation error, got %v", err)
	}
}

type blockingRDS struct{ *fakeRDS }

func (b *blockingRDS) DescribeDBInstances(ctx context.Context, _ *awsrds.DescribeDBInstancesInput,
	_ ...func(*awsrds.Options)) (*awsrds.DescribeDBInstancesOutput, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestInventoryErrorFailsTheCollectionAndOptionalErrorsDoNot(t *testing.T) {
	boom := errors.New("throttled")

	// The inventory is the one hard dependency.
	a := newRDSAPI(&fakeRDS{err: boom}, "us-east-1")
	cfg := krds.DefaultCollectorConfig(krds.Window{Start: refNow.Add(-24 * time.Hour), End: refNow})
	c, _ := krds.NewCollector(a, nil, nil, cfg)
	if _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("a failed DescribeDBInstances must fail the collection")
	}

	// A failed reservation read degrades into a warning instead.
	f := &fakeRDS{instances: []rdstypes.DBInstance{fullInstance()}}
	good := newRDSAPI(f, "us-east-1")
	bad := newRDSAPI(&errAfterInventory{fakeRDS: f, err: boom}, "us-east-1")
	c2, _ := krds.NewCollector(good, nil, bad, cfg)
	snap, err := c2.Collect(context.Background())
	if err != nil {
		t.Fatalf("a failed optional seam must not fail the collection: %v", err)
	}
	if !hasNote(snap.Warnings, "could not list Reserved DB Instances") {
		t.Fatalf("the degradation must be visible in the snapshot: %v", snap.Warnings)
	}
}

type errAfterInventory struct {
	*fakeRDS
	err error
}

func (e *errAfterInventory) DescribeReservedDBInstances(ctx context.Context,
	_ *awsrds.DescribeReservedDBInstancesInput,
	_ ...func(*awsrds.Options)) (*awsrds.DescribeReservedDBInstancesOutput, error) {
	return nil, e.err
}

// ---- access-denied classification ----

func TestIsAccessDenied(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		denied bool
	}{
		{"AccessDenied", apiErr{"AccessDenied", "not authorized"}, true},
		{"AccessDeniedException", apiErr{"AccessDeniedException", "no"}, true},
		{"wrapped", fmt.Errorf("provider rds: %w", apiErr{"AccessDenied", "no"}), true},
		// These must stay errors: a throttle turned into "you lack the
		// permission" produces a permanently degraded report from a transient
		// fault.
		{"throttling", apiErr{"ThrottlingException", "slow down"}, false},
		{"validation", apiErr{"InvalidParameterValue", "bad marker"}, false},
		{"not found", apiErr{"DBInstanceNotFound", "gone"}, false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAccessDenied(tc.err); got != tc.denied {
				t.Fatalf("IsAccessDenied(%v) = %v, want %v", tc.err, got, tc.denied)
			}
		})
	}
}

// ---- notes ----

func TestNotesAreDeduplicatedSortedAndRaceSafe(t *testing.T) {
	a := newRDSAPI(&fakeRDS{}, "us-east-1")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.notes.add("zebra")
			a.notes.add("alpha")
			a.notes.add("note %d", i%2)
			_ = a.Notes()
		}(i)
	}
	wg.Wait()
	got := a.Notes()
	want := []string{"alpha", "note 0", "note 1", "zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notes = %v, want %v", got, want)
	}
	if fresh := newRDSAPI(&fakeRDS{}, "x").Notes(); fresh != nil {
		t.Fatalf("a clean run must report no notes, got %v", fresh)
	}
}

func TestNewRDSAPIRequiresARegion(t *testing.T) {
	if _, err := NewRDSAPI(context.Background(), "  "); err == nil {
		t.Fatal("a blank region must be refused: it selects the rate-card row for every instance")
	}
	if _, err := NewCloudWatchAPI(context.Background(), ""); err == nil {
		t.Fatal("a blank region must be refused")
	}
}

// ---- helpers ----

func collectEnvelope(t *testing.T, a *RDSAPI, ids ...string) krds.Envelopes {
	t.Helper()
	ec := krds.NewEnvelopeCollector(a, krds.EnvelopeCollectorConfig{
		Window: krds.Window{Start: refNow.Add(-48 * time.Hour), End: refNow},
	})
	env, err := ec.Collect(context.Background(), ids)
	if err != nil {
		t.Fatalf("envelope collect: %v", err)
	}
	return env
}

func countCalls(f *fakeRDS, op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

func hasNote(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}
