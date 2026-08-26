package ec2

// Shared scaffolding for the U7 tests: a fake clock, a small but realistic
// instance-type catalog, a step builder, and an approval helper.
//
// Everything here is data. No test in this package can reach an AWS account:
// the package links no SDK, and every seam is [ActuateFixture].

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/model"
)

// actBase is the wall clock every actuation test starts from.
var actBase = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

const actScope = "1234/us-east-1"

// actClock is a clock the tests move, so no test spends real time.
type actClock struct {
	mu sync.Mutex
	t  time.Time
}

func newActClock(t time.Time) *actClock { return &actClock{t: t} }

func (c *actClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *actClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func actMem(gib int64) int64 { return gib << 30 }

// itoa64 renders an integer for a spec attribute; a negative value becomes the
// blank marker so a fuzz input can express "the attribute is absent".
func itoa64(v int64) string {
	if v < 0 {
		return "-"
	}
	return strconv.FormatInt(v, 10)
}

// actTypes is the catalog the fixture answers DescribeInstanceTypes from.
//
// It is deliberately mixed-generation: m5/m6i are Nitro (ENA and NVMe
// required), t2 is Xen-era (ENA merely supported, NVMe unsupported), m7g is
// Graviton, m5d carries instance storage. Every ENA/NVMe refusal below is a
// real pair drawn from this table rather than an invented flag.
func actTypes() []InstanceTypeInfo {
	nitro := func(name string, vcpu int32, memMiB int64, arch string) InstanceTypeInfo {
		return InstanceTypeInfo{
			InstanceType: name, ENASupport: SupportRequired, NVMeSupport: SupportRequired,
			SupportedArchitectures:       []string{arch},
			SupportedVirtualizationTypes: []string{"hvm"},
			SupportedRootDeviceTypes:     []string{RootDeviceEBS},
			SupportedUsageClasses:        []string{"on-demand", "spot"},
			VCPU:                         vcpu, MemoryMiB: memMiB, CurrentGeneration: true,
		}
	}
	xen := func(name string, vcpu int32, memMiB int64) InstanceTypeInfo {
		t := nitro(name, vcpu, memMiB, "x86_64")
		t.ENASupport, t.NVMeSupport = SupportSupported, SupportUnsupported
		t.SupportedVirtualizationTypes = []string{"hvm", "paravirtual"}
		t.CurrentGeneration = false
		return t
	}
	m5d := nitro("m5d.2xlarge", 8, 32768, "x86_64")
	m5d.InstanceStorageSupported, m5d.InstanceStorageTotalGB = true, 300
	metal := nitro("m5.metal", 96, 393216, "x86_64")
	metal.BareMetal = true
	unknownENA := nitro("x9.2xlarge", 8, 32768, "x86_64")
	unknownENA.ENASupport = ""
	unknownNVMe := nitro("x8.2xlarge", 8, 32768, "x86_64")
	unknownNVMe.NVMeSupport = ""
	return []InstanceTypeInfo{
		nitro("m5.2xlarge", 8, 32768, "x86_64"),
		nitro("m6i.2xlarge", 8, 32768, "x86_64"),
		nitro("m6i.xlarge", 4, 16384, "x86_64"),
		nitro("m7g.2xlarge", 8, 32768, "arm64"),
		xen("t2.2xlarge", 8, 32768),
		xen("t2.large", 2, 8192),
		m5d, metal, unknownENA, unknownNVMe,
	}
}

// actImages is the AMI table.
func actImages() []ImageDetail {
	return []ImageDetail{
		{ImageID: "ami-nitro", State: "available", Architecture: "x86_64", ENASupport: true,
			SriovNetSupport: "simple", RootDeviceType: RootDeviceEBS, VirtualizationType: "hvm"},
		{ImageID: "ami-noena", State: "available", Architecture: "x86_64", ENASupport: false,
			RootDeviceType: RootDeviceEBS, VirtualizationType: "hvm"},
		{ImageID: "ami-arm", State: "available", Architecture: "arm64", ENASupport: true,
			RootDeviceType: RootDeviceEBS, VirtualizationType: "hvm"},
		{ImageID: "ami-pv", State: "available", Architecture: "x86_64", ENASupport: true,
			RootDeviceType: RootDeviceEBS, VirtualizationType: "paravirtual"},
		{ImageID: "ami-pending", State: "pending", Architecture: "x86_64", ENASupport: true,
			RootDeviceType: RootDeviceEBS, VirtualizationType: "hvm"},
	}
}

// actInstance is a healthy, resizable, on-demand instance: the baseline every
// refusal test mutates exactly one field of.
func actInstance(id string) InstanceDetail {
	return InstanceDetail{
		InstanceID: id, InstanceType: "m5.2xlarge", State: StateRunning,
		Architecture: "x86_64", ImageID: "ami-nitro", AvailabilityZone: "us-east-1a",
		Tenancy: "default", EnaSupport: true, SriovNetSupport: "simple",
		RootDeviceType: RootDeviceEBS, ShutdownBehavior: ShutdownStop,
		BlockDevices: []BlockDevice{{DeviceName: "/dev/xvda", VolumeID: "vol-root", VolumeType: "gp3", SizeGiB: 100}},
		Tags:         []Tag{{Key: TagName, Value: "app"}},
	}
}

// --- step construction ------------------------------------------------------

// actStepOpts is the knob set for [actStep]. Its zero value builds the
// canonical healthy step: m5.2xlarge → m6i.2xlarge, same shape, same storage,
// a positive commitment-checked net saving and a memory signal.
type actStepOpts struct {
	id       string
	scope    string
	action   domain.ActionClass
	fromType string
	toType   string
	fromCPU  int64
	toCPU    int64
	fromMem  int64
	toMem    int64
	fromStor string
	toStor   string
	memSig   string
	net      string
	gross    string
	checked  string
	tenancy  [2]string
	platform [2]string
	seq      int
}

func actStep(o actStepOpts) domain.Step {
	def := func(s, d string) string {
		if s == "" {
			return d
		}
		return s
	}
	defI := func(v, d int64) int64 {
		if v == 0 {
			return d
		}
		return v
	}
	id := def(o.id, "i-app")
	scope := def(o.scope, actScope)
	action := o.action
	if action == "" {
		action = domain.ActionStopStart
	}
	ref := domain.TargetRef{Domain: Kind, Scope: scope, ID: id, Name: "app"}

	from := domain.Spec{
		Resources: model.Resources{MilliCPU: defI(o.fromCPU, 8000), MemoryBytes: defI(o.fromMem, actMem(32))},
		Attrs: map[string]string{
			AttrInstanceType: def(o.fromType, "m5.2xlarge"),
			AttrArch:         "amd64",
			AttrPlatform:     def(o.platform[0], "Linux/UNIX"),
			AttrTenancy:      def(o.tenancy[0], "default"),
			AttrStorageGiB:   def(o.fromStor, "100"),
		},
	}
	to := domain.Spec{
		Resources: model.Resources{MilliCPU: defI(o.toCPU, 8000), MemoryBytes: defI(o.toMem, actMem(32))},
		Attrs: map[string]string{
			AttrInstanceType:           def(o.toType, "m6i.2xlarge"),
			AttrArch:                   "amd64",
			AttrPlatform:               def(o.platform[1], "Linux/UNIX"),
			AttrTenancy:                def(o.tenancy[1], "default"),
			AttrStorageGiB:             def(o.toStor, "100"),
			AttrMemorySignal:           def(o.memSig, MemorySignalCWAgent),
			AttrNetSavingsMonthlyUSD:   def(o.net, "42.50"),
			AttrGrossSavingsMonthlyUSD: def(o.gross, "61.00"),
			AttrCommitmentCheckedAt:    def(o.checked, actBase.Add(-time.Hour).Format(time.RFC3339)),
		},
	}
	// A caller can blank an attribute by passing "-": there is no other way to
	// express "absent" through a string default.
	for _, s := range []domain.Spec{from, to} {
		for k, v := range s.Attrs {
			if v == "-" {
				delete(s.Attrs, k)
			}
		}
	}
	seq := o.seq
	if seq == 0 {
		seq = 1
	}
	step := domain.Step{
		Seq: seq, Target: ref, Action: action, From: from, To: to, Risk: RiskMedium,
		Detail: fmt.Sprintf("resize %s %s → %s", id, from.Attr(AttrInstanceType), to.Attr(AttrInstanceType)),
	}
	step.Key = domain.StepKey(ref, from, to)
	return step
}

// actASGStep builds the rolling step for a group.
func actASGStep(o actStepOpts) domain.Step {
	o.action = domain.ActionRolling
	if o.id == "" {
		o.id = "asg-app"
	}
	return actStep(o)
}

// --- approval ---------------------------------------------------------------

// actToken mints a token for a plan, valid for a day from the clock.
func actToken(steps []domain.Step, now time.Time) ApprovalToken {
	return ApprovalToken{
		Fingerprint: PlanFingerprint(steps),
		Scope:       actScope,
		ApprovedBy:  "operator@example.com",
		ApprovedAt:  now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}
}

// actApprove is the whole human-in-the-loop gate, condensed for a test.
func actApprove(t *testing.T, now time.Time, steps ...domain.Step) Approval {
	t.Helper()
	ap, err := NewApproval(steps, actToken(steps, now), now)
	if err != nil {
		t.Fatalf("NewApproval: %v", err)
	}
	return ap
}

// actAuthorized approves one step and authorizes it in a single call.
func actAuthorized(t *testing.T, now time.Time, step domain.Step) ApprovedStep {
	t.Helper()
	as, err := actApprove(t, now, step).Authorize(step)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return as
}

// --- actuator ---------------------------------------------------------------

// newActFixture builds the standard one-instance account.
func newActFixture(clock *actClock, insts ...InstanceDetail) *ActuateFixture {
	if len(insts) == 0 {
		insts = []InstanceDetail{actInstance("i-app")}
	}
	return NewActuateFixture(clock.Now, insts, actTypes(), actImages())
}

// newActActuator wires an actuator whose sleeps advance the fake clock instead
// of spending time.
func newActActuator(t *testing.T, f *ActuateFixture, clock *actClock, mode Mode, tune func(*ActuatorConfig)) *Actuator {
	t.Helper()
	cfg := ActuatorConfig{
		Mode:         mode,
		Now:          clock.Now,
		Logger:       slog.New(slog.DiscardHandler),
		CallTimeout:  time.Second,
		PollInterval: time.Second,
		PollTimeout:  20 * time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			clock.Advance(d)
			return ctx.Err()
		},
	}
	if tune != nil {
		tune(&cfg)
	}
	a, err := NewActuator(f, f, cfg)
	if err != nil {
		t.Fatalf("NewActuator: %v", err)
	}
	return a
}

// --- assertions -------------------------------------------------------------

// wantRefusal asserts that err is a refusal carrying exactly code, and that
// the account was not touched.
func wantRefusal(t *testing.T, f *ActuateFixture, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected refusal %q, got nil", code)
	}
	if !IsRefusal(err) {
		t.Fatalf("expected a refusal, got %T: %v", err, err)
	}
	if got := RefusalCode(err); got != code {
		t.Fatalf("refusal code = %q, want %q (%v)", got, code, err)
	}
	if f != nil {
		if n := f.Mutations(); n != 0 {
			t.Fatalf("a refusal issued %d mutating call(s): %v", n, f.Ops())
		}
	}
}

// runningAs asserts the live instance is running as the named type.
func runningAs(t *testing.T, f *ActuateFixture, id, instanceType string) {
	t.Helper()
	live, ok := f.Instance(id)
	if !ok {
		t.Fatalf("%s vanished", id)
	}
	if live.State != StateRunning || live.InstanceType != instanceType {
		t.Fatalf("%s is %s/%s, want running/%s", id, live.InstanceType, live.State, instanceType)
	}
}
