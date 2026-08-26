package ec2

// The pre-flight refusal matrix.
//
// One test per predicate, each asserting the SPECIFIC machine-readable code
// and — through [wantRefusal] — that the account was not touched. A refusal
// that issues a cloud call is not a refusal.

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// preflight runs the read-only gate for a step against a fixture.
func preflight(t *testing.T, f *ActuateFixture, clock *actClock, step domain.Step) error {
	t.Helper()
	a := newActActuator(t, f, clock, ModeDryRun, nil)
	return a.Preflight(t.Context(), step)
}

// --- §3.3: never stop an instance with instance-store data ------------------

func TestRefusesInstanceStoreVolumes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(*InstanceDetail)
	}{
		{"counted ephemeral volumes", func(d *InstanceDetail) { d.InstanceStoreVolumes = 2 }},
		{"ephemeral block device mapping", func(d *InstanceDetail) {
			d.BlockDevices = append(d.BlockDevices, BlockDevice{DeviceName: "/dev/sdb", VirtualName: "ephemeral0"})
		}},
		{"instance-store root device", func(d *InstanceDetail) { d.RootDeviceType = RootDeviceInstanceStore }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			inst := actInstance("i-app")
			tc.shape(&inst)
			f := newActFixture(clock, inst)
			wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{})), RefuseInstanceStore)
		})
	}
}

// The catalog knows some types carry ephemeral disks even when the instance
// record forgot to say so. Moving OFF such a type still destroys data.
func TestRefusesInstanceStoreDeclaredByTheCatalog(t *testing.T) {
	clock := newActClock(actBase)
	inst := actInstance("i-app")
	inst.InstanceType = "m5d.2xlarge"
	f := newActFixture(clock, inst)
	step := actStep(actStepOpts{fromType: "m5d.2xlarge"})
	wantRefusal(t, f, preflight(t, f, clock, step), RefuseInstanceStore)
}

// --- §6 U7: ENA / NVMe prerequisites ----------------------------------------

func TestRefusesMissingENAPrerequisite(t *testing.T) {
	t.Run("instance attribute is false", func(t *testing.T) {
		clock := newActClock(actBase)
		inst := actInstance("i-app")
		inst.EnaSupport = false
		f := newActFixture(clock, inst)
		wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{})), RefuseENAUnsupported)
	})
	t.Run("AMI does not declare ENA", func(t *testing.T) {
		clock := newActClock(actBase)
		inst := actInstance("i-app")
		inst.ImageID = "ami-noena"
		f := newActFixture(clock, inst)
		wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{})), RefuseENAUnsupported)
	})
	t.Run("the API did not say", func(t *testing.T) {
		clock := newActClock(actBase)
		f := newActFixture(clock)
		// x9.2xlarge has an empty ENASupport: absence of evidence refuses.
		wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{toType: "x9.2xlarge"})), RefuseENAUnsupported)
	})
}

func TestRefusesMissingNVMePrerequisite(t *testing.T) {
	t.Run("moving from a Xen-era type onto a Nitro one", func(t *testing.T) {
		clock := newActClock(actBase)
		inst := actInstance("i-app")
		inst.InstanceType = "t2.2xlarge"
		f := newActFixture(clock, inst)
		// t2's NVMe support is "unsupported", so nothing in the account shows
		// this AMI has the NVMe driver m6i requires.
		step := actStep(actStepOpts{fromType: "t2.2xlarge"})
		wantRefusal(t, f, preflight(t, f, clock, step), RefuseNVMeUnsupported)
	})
	t.Run("the API did not say", func(t *testing.T) {
		clock := newActClock(actBase)
		f := newActFixture(clock)
		wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{toType: "x8.2xlarge"})), RefuseNVMeUnsupported)
	})
	t.Run("Nitro to Nitro is evidence, not inference", func(t *testing.T) {
		clock := newActClock(actBase)
		f := newActFixture(clock)
		if err := preflight(t, f, clock, actStep(actStepOpts{})); err != nil {
			t.Fatalf("m5 → m6i must pass the NVMe gate: %v", err)
		}
	})
}

// --- §3.3: unknown or terminating shutdown behavior -------------------------

func TestRefusesUnsafeShutdownBehavior(t *testing.T) {
	for _, tc := range []struct{ name, behavior string }{
		{"terminate", ShutdownTerminate},
		{"unreadable", ""},
		{"nonsense", "reboot-maybe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			inst := actInstance("i-app")
			inst.ShutdownBehavior = tc.behavior
			f := newActFixture(clock, inst)
			wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{})), RefuseShutdownBehavior)
		})
	}
}

// --- §3.3: never reduce storage ---------------------------------------------

func TestRefusesStorageReduction(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	step := actStep(actStepOpts{fromStor: "500", toStor: "100"})
	wantRefusal(t, f, preflight(t, f, clock, step), RefuseStorageShrink)
}

func TestRefusesUndeclaredStorage(t *testing.T) {
	for _, tc := range []struct{ name, from, to string }{
		{"absent on the target", "100", "-"},
		{"absent on the source", "-", "100"},
		{"not a number", "100", "lots"},
		{"negative", "100", "-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newActFixture(clock)
			step := actStep(actStepOpts{fromStor: tc.from, toStor: tc.to})
			wantRefusal(t, f, preflight(t, f, clock, step), RefuseStorageShrink)
		})
	}
}

// --- §7 trap 1: commitment stranding ----------------------------------------

func TestRefusesCommitmentNegativeRecommendation(t *testing.T) {
	for _, tc := range []struct{ name, net, gross string }{
		{"the bill goes up", "-31.20", "18.40"},
		{"the saving is entirely absorbed", "0", "18.40"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newActFixture(clock)
			step := actStep(actStepOpts{net: tc.net, gross: tc.gross})
			wantRefusal(t, f, preflight(t, f, clock, step), RefuseCommitmentNegative)
			// Nothing was read either: the economics gate is pure and runs
			// before the first describe.
			if n := f.Count(OpDescribeInstance); n != 0 {
				t.Errorf("a commitment-negative step read the account %d time(s)", n)
			}
		})
	}
}

func TestRefusesUncheckedCommitments(t *testing.T) {
	stale := actBase.Add(-CommitmentMaxAge - time.Hour).Format(time.RFC3339)
	future := actBase.Add(48 * time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		o    actStepOpts
	}{
		{"nobody checked", actStepOpts{checked: "-"}},
		{"the check is stale", actStepOpts{checked: stale}},
		{"the check is dated in the future", actStepOpts{checked: future}},
		{"the check is not a time", actStepOpts{checked: "last tuesday"}},
		{"no net attestation", actStepOpts{net: "-"}},
		{"no gross attestation", actStepOpts{gross: "-"}},
		{"the net is not a number", actStepOpts{net: "cheap"}},
		{"the net is not finite", actStepOpts{net: "NaN"}},
		{"net exceeds gross", actStepOpts{net: "90", gross: "12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newActFixture(clock)
			wantRefusal(t, f, preflight(t, f, clock, actStep(tc.o)), RefuseCommitmentUnchecked)
		})
	}
}

// --- §7 trap 4: memory-blind downsizing -------------------------------------

func TestRefusesMemoryReductionWithoutAMemorySignal(t *testing.T) {
	shrink := actStepOpts{toType: "m6i.xlarge", toCPU: 4000, toMem: actMem(16)}
	for _, tc := range []struct{ name, signal string }{
		{"declared memory-blind", MemorySignalNone},
		{"no signal declared at all", "-"},
		{"an unrecognized signal", "vibes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			f := newActFixture(clock)
			o := shrink
			o.memSig = tc.signal
			wantRefusal(t, f, preflight(t, f, clock, actStep(o)), RefuseMemoryBlind)
		})
	}
	t.Run("a CloudWatch-agent signal unlocks it", func(t *testing.T) {
		clock := newActClock(actBase)
		f := newActFixture(clock)
		o := shrink
		o.memSig = MemorySignalCWAgent
		if err := preflight(t, f, clock, actStep(o)); err != nil {
			t.Fatalf("a memory-backed downsize must pass: %v", err)
		}
	})
	t.Run("same-or-more memory needs no signal", func(t *testing.T) {
		clock := newActClock(actBase)
		f := newActFixture(clock)
		if err := preflight(t, f, clock, actStep(actStepOpts{memSig: MemorySignalNone})); err != nil {
			t.Fatalf("a same-memory move must pass while memory-blind: %v", err)
		}
	})
}

// --- §3.3 "Never": ownership -------------------------------------------------

func TestRefusesInstancesThisDomainDoesNotOwn(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  Tag
		code string
	}{
		{"kubernetes cluster tag", Tag{Key: TagK8sClusterPrefix + "prod", Value: "owned"}, RefuseK8sTagged},
		{"eks cluster tag", Tag{Key: TagEKSCluster, Value: "prod"}, RefuseK8sTagged},
		{"aws eks cluster tag", Tag{Key: TagAWSEKSCluster, Value: "prod"}, RefuseK8sTagged},
		{"operator opted out", Tag{Key: TagKilterMode, Value: "off"}, RefuseModeOff},
		{"launched by an ASG", Tag{Key: TagASGName, Value: "asg-app"}, RefuseASGMember},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			inst := actInstance("i-app")
			inst.Tags = append(inst.Tags, tc.tag)
			f := newActFixture(clock, inst)
			wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{})), tc.code)
		})
	}
}

// --- shape of the request ----------------------------------------------------

func TestRefusesMalformedSteps(t *testing.T) {
	clock := newActClock(actBase)
	f := newActFixture(clock)
	a := newActActuator(t, f, clock, ModeDryRun, nil)

	t.Run("wrong action class", func(t *testing.T) {
		step := actStep(actStepOpts{})
		step.Action = domain.ActionInPlace
		step.Key = domain.StepKey(step.Target, step.From, step.To)
		wantRefusal(t, f, a.Preflight(t.Context(), step), RefuseWrongAction)
	})
	t.Run("a key that does not hash its contents", func(t *testing.T) {
		step := actStep(actStepOpts{})
		step.Key = "deadbeefdeadbeef"
		wantRefusal(t, f, a.Preflight(t.Context(), step), RefuseBadStep)
	})
	t.Run("a step edited after it was built", func(t *testing.T) {
		step := actStep(actStepOpts{})
		step.To = step.To.WithAttr(AttrNetSavingsMonthlyUSD, "9999")
		wantRefusal(t, f, a.Preflight(t.Context(), step), RefuseBadStep)
	})
	t.Run("no change", func(t *testing.T) {
		wantRefusal(t, f, a.Preflight(t.Context(), actStep(actStepOpts{toType: "m5.2xlarge"})), RefuseNoChange)
	})
	t.Run("changes tenancy", func(t *testing.T) {
		wantRefusal(t, f, a.Preflight(t.Context(),
			actStep(actStepOpts{tenancy: [2]string{"default", "dedicated"}})), RefuseTenancy)
	})
	t.Run("changes platform", func(t *testing.T) {
		wantRefusal(t, f, a.Preflight(t.Context(),
			actStep(actStepOpts{platform: [2]string{"Linux/UNIX", "Windows"}})), RefuseBadStep)
	})
	t.Run("no target", func(t *testing.T) {
		step := actStep(actStepOpts{})
		step.Target.ID = ""
		step.Key = domain.StepKey(step.Target, step.From, step.To)
		wantRefusal(t, f, a.Preflight(t.Context(), step), RefuseBadStep)
	})
	t.Run("shape disagrees with the catalog", func(t *testing.T) {
		wantRefusal(t, f, a.Preflight(t.Context(),
			actStep(actStepOpts{toMem: actMem(64)})), RefuseBadStep)
	})
}

func TestRefusesInstancesThisUnitDoesNotResize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(*InstanceDetail)
		step  actStepOpts
		code  string
	}{
		{"spot", func(d *InstanceDetail) { d.SpotInstanceRequestID = "sir-1"; d.LifecycleType = "spot" },
			actStepOpts{}, RefuseSpot},
		{"hibernation configured", func(d *InstanceDetail) { d.HibernationConfigured = true },
			actStepOpts{}, RefuseHibernation},
		{"dedicated tenancy", func(d *InstanceDetail) { d.Tenancy = "dedicated" },
			actStepOpts{tenancy: [2]string{"dedicated", "dedicated"}}, RefuseTenancy},
		{"bare metal target", func(d *InstanceDetail) {},
			actStepOpts{toType: "m5.metal", toCPU: 96000, toMem: actMem(384)}, RefuseBareMetal},
		{"architecture change", func(d *InstanceDetail) {},
			actStepOpts{toType: "m7g.2xlarge"}, RefuseArchMismatch},
		{"paravirtual AMI", func(d *InstanceDetail) { d.ImageID = "ami-pv" },
			actStepOpts{}, RefuseVirtualization},
		{"AMI is gone", func(d *InstanceDetail) { d.ImageID = "ami-deregistered" },
			actStepOpts{}, RefuseImageMissing},
		{"AMI is not available", func(d *InstanceDetail) { d.ImageID = "ami-pending" },
			actStepOpts{}, RefuseImageMissing},
		{"instance reports no AMI", func(d *InstanceDetail) { d.ImageID = "" },
			actStepOpts{}, RefuseImageMissing},
		{"unknown target type", func(d *InstanceDetail) {},
			actStepOpts{toType: "q9.enormous"}, RefuseUnknownInstanceType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newActClock(actBase)
			inst := actInstance("i-app")
			tc.shape(&inst)
			f := newActFixture(clock, inst)
			wantRefusal(t, f, preflight(t, f, clock, actStep(tc.step)), tc.code)
		})
	}
}

func TestRefusesInstancesThatAreNotThere(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		clock := newActClock(actBase)
		f := newActFixture(clock)
		wantRefusal(t, f, preflight(t, f, clock, actStep(actStepOpts{id: "i-ghost"})), RefuseInstanceMissing)
	})
	for _, state := range []string{StateTerminated, StateShuttingDown} {
		t.Run(state, func(t *testing.T) {
			clock := newActClock(actBase)
			inst := actInstance("i-app")
			inst.State = state
			f := newActFixture(clock, inst)
			a := newActActuator(t, f, clock, ModeApply, nil)
			as := actAuthorized(t, actBase, actStep(actStepOpts{}))
			wantRefusal(t, f, a.Execute(t.Context(), as), RefuseInstanceState)
		})
	}
}

// Drift: the live instance is neither the recorded From nor the intended To.
func TestRefusesDriftedInstance(t *testing.T) {
	clock := newActClock(actBase)
	inst := actInstance("i-app")
	inst.InstanceType = "t2.2xlarge" // somebody else already changed it
	f := newActFixture(clock, inst)
	a := newActActuator(t, f, clock, ModeApply, nil)
	as := actAuthorized(t, actBase, actStep(actStepOpts{}))
	wantRefusal(t, f, a.Execute(t.Context(), as), RefuseDrift)
}

// Dry-run and apply must refuse for identical reasons. An apply that can do
// something a dry-run never showed is the bug class this symmetry exists to
// prevent — pkg/ebs's actuator makes the same promise for the same reason.
func TestDryRunAndApplyRefuseIdentically(t *testing.T) {
	cases := []struct {
		name string
		o    actStepOpts
		mut  func(*InstanceDetail)
	}{
		{"instance store", actStepOpts{}, func(d *InstanceDetail) { d.InstanceStoreVolumes = 1 }},
		{"shutdown behavior", actStepOpts{}, func(d *InstanceDetail) { d.ShutdownBehavior = ShutdownTerminate }},
		{"commitment negative", actStepOpts{net: "-1"}, nil},
		{"memory blind", actStepOpts{toType: "m6i.xlarge", toCPU: 4000, toMem: actMem(16), memSig: MemorySignalNone}, nil},
		{"storage shrink", actStepOpts{toStor: "1"}, nil},
		{"ena", actStepOpts{}, func(d *InstanceDetail) { d.EnaSupport = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codes := map[Mode]string{}
			for _, mode := range []Mode{ModeDryRun, ModeApply} {
				clock := newActClock(actBase)
				inst := actInstance("i-app")
				if tc.mut != nil {
					tc.mut(&inst)
				}
				f := newActFixture(clock, inst)
				a := newActActuator(t, f, clock, mode, nil)
				step := actStep(tc.o)
				err := a.Execute(t.Context(), actAuthorized(t, actBase, step))
				wantRefusal(t, f, err, RefusalCode(err))
				codes[mode] = RefusalCode(err)
				e, ok := a.Entry(step.Key)
				if !ok || e.Status != StatusRefused || e.RefusalCode == "" {
					t.Fatalf("ledger entry = %+v, want a refused entry carrying its code", e)
				}
			}
			if codes[ModeDryRun] != codes[ModeApply] {
				t.Fatalf("dry-run refused %q, apply refused %q", codes[ModeDryRun], codes[ModeApply])
			}
			if codes[ModeApply] == "" {
				t.Fatal("neither mode produced a refusal code")
			}
		})
	}
}
