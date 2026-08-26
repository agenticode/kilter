package ecs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// testNow is the single decision time every test uses. Nothing in this package
// reads a clock, so a constant is enough.
var testNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// testConfig lowers the evidence floors so a fixture does not have to carry two
// weeks of 1-minute datapoints to be assessed. The shipped defaults are
// asserted separately by TestDefaultConfigFloors, which does carry them.
func testConfig() Config {
	c := DefaultConfig()
	c.MinWindow = 24 * time.Hour
	c.MinSamples = 100
	c.Region = "us-east-1"
	return c
}

const (
	testCluster = "prod"
	testService = "web"
	testTDARN   = "arn:aws:ecs:us-east-1:111122223333:task-definition/web:7"
	testScope   = "111122223333/us-east-1"
)

// fixture builds one Observation. Every field has a realistic default; a test
// states only what it is about.
type fixture struct {
	cluster, service string
	// cpu and memory as the task definition declares them.
	cpu, memory     string
	tasks           int
	running         int
	pending         int
	arch, osFamily  string
	platformVersion string
	launchType      string
	strategy        []CapacityProviderItem
	tags            []Tag
	networkMode     string
	containers      []ContainerDefinition
	deployments     []Deployment
	// cpuPct and memPct are percent-of-reserved series; nil means "constant".
	cpuPct, memPct         []float64
	constCPUPct, constMem  float64
	samples                int
	period                 time.Duration
	cpuStatus, memStatus   string
	tdARN                  string
	revision               int
	badSizeStrings         bool
	endOffset              time.Duration
	suppressPrimaryDeploy  bool
	primaryDeployCreatedAt time.Time
}

func newFixture(mods ...func(*fixture)) *fixture {
	f := &fixture{
		cluster:         testCluster,
		service:         testService,
		cpu:             "4096", // 4 vCPU
		memory:          "8192", // 8 GiB
		tasks:           4,
		running:         -1, // -1 ⇒ mirror tasks
		arch:            string(ArchX86),
		osFamily:        "LINUX",
		platformVersion: "LATEST",
		launchType:      LaunchTypeFargate,
		networkMode:     NetworkModeAWSVPC,
		constCPUPct:     30,
		constMem:        30,
		samples:         400,
		period:          10 * time.Minute,
		tdARN:           testTDARN,
		revision:        7,
	}
	for _, m := range mods {
		m(f)
	}
	return f
}

func (f *fixture) service_() ServiceRecord {
	running := f.running
	if running < 0 {
		running = f.tasks
	}
	deps := f.deployments
	if deps == nil && !f.suppressPrimaryDeploy {
		created := f.primaryDeployCreatedAt
		if created.IsZero() {
			created = testNow.Add(-30 * 24 * time.Hour)
		}
		deps = []Deployment{{
			ID:             "ecs-svc/1",
			Status:         DeploymentPrimary,
			TaskDefinition: f.tdARN,
			DesiredCount:   f.tasks,
			RunningCount:   running,
			PendingCount:   f.pending,
			RolloutState:   RolloutCompleted,
			CreatedAt:      created,
			UpdatedAt:      created,
		}}
	}
	return ServiceRecord{
		ServiceARN:               "arn:aws:ecs:us-east-1:111122223333:service/" + f.cluster + "/" + f.service,
		ServiceName:              f.service,
		ClusterARN:               "arn:aws:ecs:us-east-1:111122223333:cluster/" + f.cluster,
		Status:                   "ACTIVE",
		LaunchType:               f.launchType,
		CapacityProviderStrategy: f.strategy,
		PlatformVersion:          f.platformVersion,
		PlatformFamily:           "Linux",
		TaskDefinition:           f.tdARN,
		DesiredCount:             f.tasks,
		RunningCount:             running,
		PendingCount:             f.pending,
		Deployments:              deps,
		Tags:                     f.tags,
	}
}

func (f *fixture) taskDef() TaskDefinitionRecord {
	cpu, mem := f.cpu, f.memory
	if f.badSizeStrings {
		cpu, mem = "lots", "plenty"
	}
	return TaskDefinitionRecord{
		TaskDefinitionARN:       f.tdARN,
		Family:                  f.service,
		Revision:                f.revision,
		Status:                  "ACTIVE",
		CPU:                     cpu,
		Memory:                  mem,
		NetworkMode:             f.networkMode,
		RequiresCompatibilities: []string{"FARGATE"},
		RuntimePlatform: RuntimePlatform{
			CPUArchitecture:       f.arch,
			OperatingSystemFamily: f.osFamily,
		},
		ContainerDefinitions: f.containers,
	}
}

// series builds a percent-of-reserved series ending at testNow.
func (f *fixture) series(metric string, values []float64, constant float64, status string) Series {
	n := f.samples
	if values != nil {
		n = len(values)
	}
	s := Series{Metric: metric, PeriodSeconds: int32(f.period / time.Second), StatusCode: status}
	for i := range n {
		ts := testNow.Add(-f.endOffset - time.Duration(n-1-i)*f.period)
		v := constant
		if values != nil {
			v = values[i]
		}
		s.Timestamps = append(s.Timestamps, ts)
		s.Values = append(s.Values, v)
	}
	return s
}

func (f *fixture) observation() Observation {
	svc := f.service_()
	td := f.taskDef()
	o := Observation{
		Ref: domain.TargetRef{
			Domain: Kind, Scope: testScope,
			ID: TargetID(f.cluster, f.service), Name: f.service,
		},
		Service:    svc,
		TaskDef:    td,
		CPUPercent: f.series(MetricCPUUtilization, f.cpuPct, f.constCPUPct, f.cpuStatus),
		MemPercent: f.series(MetricMemoryUtilization, f.memPct, f.constMem, f.memStatus),
		Window:     Window{Start: testNow.Add(-14 * 24 * time.Hour), End: testNow},
		Tags:       tagMap(svc.Tags),
	}
	if res, err := td.Reserved(); err == nil {
		o.Reserved = Reservation{Revision: td.Revision, ARN: td.TaskDefinitionARN, Reserved: res}
	}
	return o
}

func (f *fixture) snapshot() *Snapshot {
	return &Snapshot{
		Domain:    Kind,
		Scope:     testScope,
		Cluster:   f.cluster,
		Timestamp: testNow,
		Window:    Window{Start: testNow.Add(-14 * 24 * time.Hour), End: testNow},
		Services:  []Observation{f.observation()},
	}
}

// assess is the one-liner most tests want.
func assess(t *testing.T, f *fixture, mods ...func(*Config)) Assessment {
	t.Helper()
	cfg := testConfig()
	for _, m := range mods {
		m(&cfg)
	}
	return NewSizer(cfg).Assess(f.observation(), testNow, nil)
}

// hasSuppression reports whether the assessment carries a suppression code.
func hasSuppression(a Assessment, code string) bool {
	for _, s := range a.Suppressions {
		if s.Code == code {
			return true
		}
	}
	return false
}

func suppressionCodes(a Assessment) []string {
	out := make([]string, 0, len(a.Suppressions))
	for _, s := range a.Suppressions {
		out = append(out, s.Code)
	}
	return out
}

func advisory(a Assessment, code string) (Advisory, bool) {
	for _, ad := range a.Advisories {
		if ad.Code == code {
			return ad, true
		}
	}
	return Advisory{}, false
}

// --- Fake seams ------------------------------------------------------------

// fakeAPI implements InventoryAPI and MutateAPI over in-memory fixtures, and
// records every mutation so a test can assert what was sent — including that
// nothing was.
type fakeAPI struct {
	serviceARNs []string
	services    map[string]ServiceRecord // by service name
	taskDefs    map[string]TaskDefinitionRecord

	listErr, describeErr, tdErr, registerErr, updateErr error
	failures                                            []Failure
	listPages                                           [][]string

	registered []RegisterTaskDefinitionInput
	updated    []UpdateServiceInput
	nextRev    int
}

func newFakeAPI(fs ...*fixture) *fakeAPI {
	a := &fakeAPI{
		services: map[string]ServiceRecord{},
		taskDefs: map[string]TaskDefinitionRecord{},
	}
	for _, f := range fs {
		svc := f.service_()
		a.serviceARNs = append(a.serviceARNs, svc.ServiceARN)
		a.services[svc.ServiceName] = svc
		a.taskDefs[f.tdARN] = f.taskDef()
		a.nextRev = f.revision
	}
	return a
}

func (a *fakeAPI) ListServices(_ context.Context, in *ListServicesInput) (*ListServicesOutput, error) {
	if a.listErr != nil {
		return nil, a.listErr
	}
	if len(a.listPages) > 0 {
		i := 0
		if in.NextToken != "" {
			fmt.Sscanf(in.NextToken, "p%d", &i)
		}
		if i >= len(a.listPages) {
			return &ListServicesOutput{}, nil
		}
		out := &ListServicesOutput{ServiceARNs: a.listPages[i]}
		if i+1 < len(a.listPages) {
			out.NextToken = fmt.Sprintf("p%d", i+1)
		}
		return out, nil
	}
	return &ListServicesOutput{ServiceARNs: a.serviceARNs}, nil
}

func (a *fakeAPI) DescribeServices(_ context.Context, in *DescribeServicesInput) (*DescribeServicesOutput, error) {
	if a.describeErr != nil {
		return nil, a.describeErr
	}
	out := &DescribeServicesOutput{Failures: a.failures}
	for _, want := range in.Services {
		for name, s := range a.services {
			if name == want || s.ServiceARN == want {
				out.Services = append(out.Services, s)
			}
		}
	}
	return out, nil
}

func (a *fakeAPI) DescribeTaskDefinition(_ context.Context, in *DescribeTaskDefinitionInput) (*DescribeTaskDefinitionOutput, error) {
	if a.tdErr != nil {
		return nil, a.tdErr
	}
	td, ok := a.taskDefs[in.TaskDefinition]
	if !ok {
		return nil, fmt.Errorf("no such task definition %q", in.TaskDefinition)
	}
	return &DescribeTaskDefinitionOutput{TaskDefinition: td}, nil
}

func (a *fakeAPI) RegisterTaskDefinition(_ context.Context, in *RegisterTaskDefinitionInput) (*RegisterTaskDefinitionOutput, error) {
	if a.registerErr != nil {
		return nil, a.registerErr
	}
	a.registered = append(a.registered, *in)
	a.nextRev++
	td := in.TaskDefinition
	td.Revision = a.nextRev
	td.Status = "ACTIVE"
	td.TaskDefinitionARN = fmt.Sprintf("arn:aws:ecs:us-east-1:111122223333:task-definition/%s:%d",
		td.Family, td.Revision)
	a.taskDefs[td.TaskDefinitionARN] = td
	return &RegisterTaskDefinitionOutput{TaskDefinition: td}, nil
}

func (a *fakeAPI) UpdateService(_ context.Context, in *UpdateServiceInput) (*UpdateServiceOutput, error) {
	if a.updateErr != nil {
		return nil, a.updateErr
	}
	a.updated = append(a.updated, *in)
	s, ok := a.services[in.Service]
	if !ok {
		return nil, fmt.Errorf("no such service %q", in.Service)
	}
	s.TaskDefinition = in.TaskDefinition
	for i := range s.Deployments {
		if s.Deployments[i].Status == DeploymentPrimary {
			s.Deployments[i].TaskDefinition = in.TaskDefinition
		}
	}
	a.services[in.Service] = s
	return &UpdateServiceOutput{Service: s}, nil
}

// fakeMetrics serves percent-of-reserved series keyed by service name.
type fakeMetrics struct {
	series map[string]map[string]Series // service → metric → series
	err    error
	drop   map[string]bool // query IDs to omit from the response entirely
	status map[string]string
	// reverse emits results in the opposite order to the queries, which a real
	// CloudWatch response is under no obligation to preserve.
	reverse bool
	calls   int
}

func newFakeMetrics(fs ...*fixture) *fakeMetrics {
	m := &fakeMetrics{series: map[string]map[string]Series{}}
	for _, f := range fs {
		o := f.observation()
		m.series[f.service] = map[string]Series{
			MetricCPUUtilization:    o.CPUPercent,
			MetricMemoryUtilization: o.MemPercent,
		}
	}
	return m
}

func (m *fakeMetrics) GetMetricData(_ context.Context, in *GetMetricDataInput) (*GetMetricDataOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	m.calls++
	out := &GetMetricDataOutput{}
	for _, q := range in.Queries {
		if m.drop[q.ID] {
			continue
		}
		s := m.series[q.Dimensions[DimServiceName]][q.MetricName]
		status := StatusComplete
		if st, ok := m.status[q.ID]; ok {
			status = st
		}
		out.Results = append(out.Results, MetricDataResult{
			ID:         q.ID,
			Label:      q.Label,
			Timestamps: s.Timestamps,
			Values:     s.Values,
			StatusCode: status,
		})
	}
	if m.reverse {
		for i, j := 0, len(out.Results)-1; i < j; i, j = i+1, j-1 {
			out.Results[i], out.Results[j] = out.Results[j], out.Results[i]
		}
	}
	return out, nil
}
