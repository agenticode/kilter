package rds

// The refusal layer, one test per predicate.
//
// Each test drives the FULL execute path — not the pure predicate in
// isolation — in APPLY mode against a fixture that would happily modify the
// database if the pre-flight let it through. That is deliberate: a refusal
// asserted against a pure function proves the function refuses; a refusal
// asserted against the apply path proves the DATABASE was not touched. Every
// test below ends by checking f.Mutations() == 0.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// refuseWith is the shape every test here shares: apply mode, the default
// step, a fixture bent one way, and the assertion that nothing was sent.
func refuseWith(t *testing.T, f *StorageActuateFixture, step domain.Step, code string) *RefusalError {
	t.Helper()
	a := actActuator(t, f, ModeApply)
	err := actExecute(t, a, step)
	r := wantRefusal(t, err, code)
	if n := f.Mutations(); n != 0 {
		t.Fatalf("%s: %d modification(s) were issued despite the refusal", code, n)
	}
	e, ok := a.Entry(step.Key)
	if !ok {
		t.Fatalf("%s: no ledger entry was recorded; a refusal nobody can read is not a refusal", code)
	}
	if e.Status != StatusRefused {
		t.Errorf("%s: ledger status = %q, want %q", code, e.Status, StatusRefused)
	}
	if e.RefusalCode != code {
		t.Errorf("%s: ledger refusal code = %q", code, e.RefusalCode)
	}
	return r
}

// --- FINDINGS.md §5.3: the cooldown, and the unknown that must block --------

// Four modifications inside 24 hours block the fifth, and the refusal carries
// the moment it clears so a scheduler does not have to re-derive it.
func TestActuateRefusesBlockedCooldown(t *testing.T) {
	f := actFixture(t)
	oldest := actNow().Add(-20 * time.Hour)
	for i := range MaxStorageModificationsPer24h {
		f.Events[actID] = append(f.Events[actID], EventRecord{
			SourceIdentifier: actID, SourceType: EventSourceDBInstance,
			Message:    "Finished applying modification to allocated storage",
			Categories: []string{EventCategoryConfigurationChange},
			Date:       oldest.Add(time.Duration(i) * time.Hour),
		})
	}
	r := refuseWith(t, f, actDefaultStep(), RefuseCooldown)
	want := oldest.Add(StorageModificationWindow)
	if !r.ValidFrom.Equal(want) {
		t.Errorf("ValidFrom = %s, want the moment the OLDEST of the four leaves the window (%s)",
			r.ValidFrom, want)
	}
	if !strings.Contains(r.Reason, "four") {
		t.Errorf("the cooldown refusal does not quote the limit: %q", r.Reason)
	}
}

// FINDINGS.md §5.3, the sentence this whole unit turns on: Known=false MUST
// block. An unreadable history is not an empty one.
func TestActuateRefusesUnknownCooldown(t *testing.T) {
	f := actFixture(t)
	f.EventsErr = map[string]error{actID: errors.New("AccessDenied: rds:DescribeEvents")}
	r := refuseWith(t, f, actDefaultStep(), RefuseCooldownUnknown)
	if !strings.Contains(r.Reason, "not zero") {
		t.Errorf("the refusal does not say that unknown is not zero: %q", r.Reason)
	}
	// And it is a DIFFERENT code from the blocked case, because the operator's
	// next action differs.
	if RefuseCooldownUnknown == RefuseCooldown {
		t.Fatal("unknown and blocked share one code; one of the two operator actions is then invisible")
	}
}

// An instance whose history is empty but READ is allowed through: the gate is
// "unknown blocks", not "silence blocks".
func TestActuateAllowsAKnownEmptyHistory(t *testing.T) {
	f := actFixture(t)
	a := actActuator(t, f, ModeDryRun)
	if err := actExecute(t, a, actDefaultStep()); err != nil {
		t.Fatalf("a known-empty history was refused: %v", err)
	}
}

// --- FINDINGS.md §5.4: the in-flight gate, re-checked live ------------------

// U13 observed this instance hours ago. It is `storage-optimization` NOW.
func TestActuateRefusesUnstableStateAtExecuteTime(t *testing.T) {
	for _, status := range []string{StatusStorageOptimization, StatusModifying} {
		t.Run(status, func(t *testing.T) {
			f := actFixture(t, func(r *InstanceStateRecord) { r.Status = status })
			r := refuseWith(t, f, actDefaultStep(), RefuseStateUnstable)
			if !strings.Contains(r.Reason, status) {
				t.Errorf("the refusal does not name the state: %q", r.Reason)
			}
		})
	}
}

// Every other non-available state refuses too, under its own code: a stopped
// instance is not mid-change, and telling an operator it is would be a lie.
func TestActuateRefusesNonAvailableState(t *testing.T) {
	for _, status := range []string{StatusStopped, "backing-up", "failing-over", "deleting", ""} {
		t.Run(orNone(status), func(t *testing.T) {
			f := actFixture(t, func(r *InstanceStateRecord) { r.Status = status })
			refuseWith(t, f, actDefaultStep(), RefuseNotAvailable)
		})
	}
}

// RDS has already accepted a change. A second one spends a second of four.
func TestActuateRefusesPendingModification(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) {
		r.PendingStorageType = StorageGP3
		r.PendingStorageThroughputMBps = 2000
	})
	r := refuseWith(t, f, actDefaultStep(), RefusePendingModification)
	if !strings.Contains(r.Reason, "four") {
		t.Errorf("the refusal does not explain the cost of a second call: %q", r.Reason)
	}
}

// --- FINDINGS.md §5.2: the envelope, re-read LIVE ---------------------------

// The case §5.2 names by hand: the instance class changed between plan and
// apply, so the envelope did too, and the configuration the plan carries no
// longer fits.
func TestActuateRefusesWhenLiveEnvelopeDisagrees(t *testing.T) {
	f := actFixture(t)
	// A smaller class: same regime, much tighter ceiling.
	f.WithEnvelope(actID, ValidStorageOptionRecord{
		StorageType: StorageGP3, MinIOPS: 12000, MaxIOPS: 12000,
		MinStorageThroughputMBps: 500, MaxStorageThroughputMBps: 700,
	})
	r := refuseWith(t, f, actDefaultStep(), RefuseExceedsEnvelope)
	if !strings.Contains(r.Reason, "class changed") {
		t.Errorf("the refusal does not name the cause re-reading exists for: %q", r.Reason)
	}
}

// An envelope nobody answered refuses. There is no "assume the published
// ceiling" path, because §2.4 names two published ceilings that disagree.
func TestActuateRefusesUnknownEnvelope(t *testing.T) {
	f := actFixture(t)
	f.EnvelopeErr = map[string]error{actID: errors.New("AccessDenied")}
	refuseWith(t, f, actDefaultStep(), RefuseEnvelopeUnknown)

	// And the same when the seam answers with nothing at all.
	g := actFixture(t)
	g.WithEnvelope(actID)
	refuseWith(t, g, actDefaultStep(), RefuseEnvelopeUnknown)
}

// Below the striping threshold gp3 IS 3,000 / 125 and there is no knob.
func TestActuateRefusesBelowTheStripingThreshold(t *testing.T) {
	const small = int64(300) // MySQL stripes at 400
	f := NewStorageActuateFixture(actClock(), actLive(func(r *InstanceStateRecord) {
		r.AllocatedStorageGiB = small
	}))
	f.WithEnvelope(actID, actGP3Envelope())
	f.WithEvents(actID)
	step := actStep(
		actSpec(actEngine, StorageGP2, small, -1, -1),
		actSpec(actEngine, StorageGP3, small, 3000, 250),
	)
	r := refuseWith(t, f, step, RefuseNotProvisionable)
	if !strings.Contains(r.Reason, "any price") {
		t.Errorf("the refusal does not say the throughput cannot be bought: %q", r.Reason)
	}
}

// --- the trap-8 ratchet: up or sideways, never down -------------------------

// The single most consequential refusal in this unit. U13 will happily propose
// reducing provisioned IOPS toward the baseline; U14 will not execute it.
func TestActuateRefusesAReduction(t *testing.T) {
	cases := []struct {
		name       string
		iops, tput int32
	}{
		{"iops", 12000, 2000},
		{"throughput", 20000, 1000},
		{"both", 12000, 500},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := actFixture(t, func(r *InstanceStateRecord) {
				r.StorageType, r.IOPS, r.StorageThroughputMBps = StorageGP3, 20000, 2000
			})
			step := actStep(
				actSpec(actEngine, StorageGP3, actSize, 20000, 2000),
				actSpec(actEngine, StorageGP3, actSize, c.iops, c.tput),
			)
			r := refuseWith(t, f, step, RefuseRatchet)
			if !strings.Contains(r.Reason, "never down") {
				t.Errorf("the refusal does not state the rule: %q", r.Reason)
			}
		})
	}
}

// The ratchet is checked against the LIVE volume, not against the step's From,
// so a plan that predates somebody else's reduction cannot reduce further.
func TestActuateRatchetIsCheckedAgainstTheLiveVolume(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) {
		// Somebody already raised it beyond what this plan targets.
		r.StorageType, r.IOPS, r.StorageThroughputMBps = StorageGP3, 12000, 2000
	})
	step := actStep(
		actSpec(actEngine, StorageGP2, actSize, -1, -1),
		actSpec(actEngine, StorageGP3, actSize, 12000, 1000),
	)
	// The live volume matches neither From (gp2) nor To (1,000 MiB/s), so
	// drift is the honest answer and it is caught BEFORE the ratchet.
	refuseWith(t, f, step, RefuseDrift)
}

// Allocated storage is a one-way ratchet whose floor never comes back down
// (trap 8). This unit never names it — not up, not down.
func TestActuateRefusesAnAllocationChange(t *testing.T) {
	for _, to := range []int64{actSize - 100, actSize + 100} {
		t.Run(strconv.FormatInt(to, 10), func(t *testing.T) {
			f := actFixture(t)
			step := actStep(
				actSpec(actEngine, StorageGP2, actSize, -1, -1),
				actSpec(actEngine, StorageGP3, to, 12000, 1000),
			)
			r := refuseWith(t, f, step, RefuseRatchet)
			if !strings.Contains(r.Reason, "trap 8") {
				t.Errorf("the refusal does not name the trap: %q", r.Reason)
			}
		})
	}
}

// The observed allocation no longer matches what the proposal was computed
// against — which is what storage autoscaling does while nobody is looking.
func TestActuateRefusesAllocationDrift(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) { r.AllocatedStorageGiB = actSize + 200 })
	r := refuseWith(t, f, actDefaultStep(), RefuseAllocationDrift)
	if !strings.Contains(r.Reason, "autoscaling") {
		t.Errorf("the refusal does not name the mechanism: %q", r.Reason)
	}
}

// --- the baseline must not be sent ------------------------------------------

// FINDINGS.md §5.1: a value equal to the regime baseline is free and needs no
// argument, and below the threshold sending one at all is an error.
//
// The property is asserted over a sweep rather than one case, because the
// interesting failure is a size where the regime changes and the call does
// not: 399 GiB and 400 GiB MySQL have different baselines and the same plan
// shape.
func TestActuateNeverSendsABaselineArgument(t *testing.T) {
	engines := []string{"mysql", "postgres", "oracle-se2", "sqlserver-se"}
	for _, raw := range engines {
		e := ParseEngine(raw, "license-included")
		for _, size := range []int64{100, 199, 200, 201, 399, 400, 401, 1000, 4000} {
			r := GP3RegimeFor(e, size)
			if !r.Known {
				continue
			}
			for _, iops := range []int32{0, r.BaselineIOPS, r.BaselineIOPS + 1000} {
				for _, tput := range []int32{0, r.BaselineThroughputMBps, r.BaselineThroughputMBps + 500} {
					cfg := configOf(r, size, iops, tput)
					gotIOPS, gotTput := argumentsFor(r, cfg)
					if !r.Provisionable && (gotIOPS != 0 || gotTput != 0) {
						t.Fatalf("%s %d GiB is not provisionable and the call would send %d IOPS / %d MiB/s",
							raw, size, gotIOPS, gotTput)
					}
					if gotIOPS != 0 && gotIOPS <= r.BaselineIOPS {
						t.Fatalf("%s %d GiB: the call would send --iops %d against a %d baseline",
							raw, size, gotIOPS, r.BaselineIOPS)
					}
					if gotTput != 0 && gotTput <= r.BaselineThroughputMBps {
						t.Fatalf("%s %d GiB: the call would send --storage-throughput %d against a %d baseline",
							raw, size, gotTput, r.BaselineThroughputMBps)
					}
				}
			}
		}
	}
}

// And the predicate that would catch it if a future edit broke the property
// above fires with its own code. A GP3Config cannot honestly claim to
// provision its own baseline — GP3Config.Validate rejects that — so this
// builds the dishonest value directly, which is exactly the state a bug
// upstream would produce.
func TestActuateRefusesABaselineArgumentByName(t *testing.T) {
	e := ParseEngine(actEngine, "general-public-license")
	r := GP3RegimeFor(e, actSize)
	in := storageIntent{ref: actRef(), allocGiB: actSize, engine: e}

	lying := GP3Config{SizeGiB: actSize, IOPS: r.BaselineIOPS, ThroughputMBps: r.BaselineThroughputMBps,
		ProvisionedIOPS: true, ProvisionedThroughput: true}
	err := checkNoBaselineArgument(in, storageFacts{regime: r, want: lying})
	rr := wantRefusal(t, err, RefuseBaselineArgument)
	if !strings.Contains(rr.Reason, "free baseline") {
		t.Errorf("the refusal does not say the value is free: %q", rr.Reason)
	}

	// Below the striping threshold NOTHING may be provisioned, and the last
	// line of defence says so under the SAME code as the earlier gate: one
	// fact, one code.
	small := GP3RegimeFor(e, 300)
	err = checkNoBaselineArgument(storageIntent{ref: actRef(), allocGiB: 300, engine: e},
		storageFacts{regime: small, want: GP3Config{SizeGiB: 300, IOPS: 9000, ThroughputMBps: 400,
			ProvisionedIOPS: true, ProvisionedThroughput: true}})
	wantRefusal(t, err, RefuseNotProvisionable)
}

// --- guardrails --------------------------------------------------------------

func TestActuateRefusesModeOff(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) { r.Tags[TagKilterMode] = "off" })
	r := refuseWith(t, f, actDefaultStep(), RefuseModeOff)
	if !strings.Contains(r.Reason, "outranks") {
		t.Errorf("the refusal does not say the tag wins: %q", r.Reason)
	}
}

// An unreadable "never touch this" is not an absent one.
func TestActuateRefusesUnreadableTags(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) { r.TagsKnown = false })
	refuseWith(t, f, actDefaultStep(), RefuseGuardrailUnknown)
}

// --- live state ---------------------------------------------------------------

func TestActuateRefusesAMissingInstance(t *testing.T) {
	f := actFixture(t)
	f.Remove(actID)
	refuseWith(t, f, actDefaultStep(), RefuseInstanceMissing)
}

func TestActuateRefusesDrift(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) {
		r.StorageType, r.IOPS, r.StorageThroughputMBps = StorageGP3, 30000, 3000
	})
	r := refuseWith(t, f, actDefaultStep(), RefuseDrift)
	if !strings.Contains(r.Reason, "no longer exists") {
		t.Errorf("the drift refusal does not say the plan is stale: %q", r.Reason)
	}
}

// The regime is engine-keyed, so an engine that changed under the plan makes
// every number in it describe another database.
func TestActuateRefusesAnEngineChange(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		f := actFixture(t, func(r *InstanceStateRecord) { r.Engine = "oracle-se2" })
		refuseWith(t, f, actDefaultStep(), RefuseEngineChanged)
	})
	t.Run("step", func(t *testing.T) {
		f := actFixture(t)
		step := actStep(
			actSpec("mysql", StorageGP2, actSize, -1, -1),
			actSpec("postgres", StorageGP3, actSize, 12000, 1000),
		)
		refuseWith(t, f, step, RefuseEngineChanged)
	})
}

func TestActuateRefusesAnUnknownEngine(t *testing.T) {
	f := actFixture(t, func(r *InstanceStateRecord) { r.Engine = "quantum-db" })
	step := actStep(
		actSpec("quantum-db", StorageGP2, actSize, -1, -1),
		actSpec("quantum-db", StorageGP3, actSize, 12000, 1000),
	)
	refuseWith(t, f, step, RefuseUnknownEngine)
}

// --- the shape of the request -------------------------------------------------

func TestActuateRefusesAMalformedStep(t *testing.T) {
	from := actSpec(actEngine, StorageGP2, actSize, -1, -1)
	to := actSpec(actEngine, StorageGP3, actSize, 12000, 1000)

	t.Run("wrong-action", func(t *testing.T) {
		s := actStep(from, to)
		s.Action = domain.ActionStopStart
		s.Key = domain.StepKey(s.Target, s.From, s.To)
		refuseWith(t, actFixture(t), s, RefuseWrongAction)
	})
	t.Run("edited-after-hashing", func(t *testing.T) {
		s := actStep(from, to)
		s.To = actSpec(actEngine, StorageGP3, actSize, 64000, 4000) // key now lies
		f := actFixture(t)
		a := actActuator(t, f, ModeApply)
		// Authorize refuses it before the actuator ever sees it; the direct
		// path is checked too, because a caller can build an ApprovedStep for
		// one step and mutate it afterwards only inside this package.
		if _, err := actApproval(t, actStep(from, to)).Authorize(s); !errors.Is(err, ErrStepKeyMismatch) {
			t.Fatalf("Authorize accepted an edited step: %v", err)
		}
		if err := a.Preflight(context.Background(), s); RefusalCode(err) != RefuseBadStep {
			t.Fatalf("Preflight code = %q, want %q (%v)", RefusalCode(err), RefuseBadStep, err)
		}
	})
	t.Run("no-change", func(t *testing.T) {
		same := actSpec(actEngine, StorageGP3, actSize, 12000, 1000)
		other := actSpec(actEngine, StorageGP3, actSize, 12000, 1000)
		other.Attrs["kilter.dev/note"] = "distinct key, same configuration"
		f := actFixture(t, func(r *InstanceStateRecord) {
			r.StorageType, r.IOPS, r.StorageThroughputMBps = StorageGP3, 12000, 1000
		})
		refuseWith(t, f, actStep(same, other), RefuseNoChange)
	})
	t.Run("unmodelled-storage-type", func(t *testing.T) {
		refuseWith(t, actFixture(t), actStep(
			actSpec(actEngine, StorageIO1, actSize, 20000, 1000),
			actSpec(actEngine, StorageGP3, actSize, 12000, 1000)), RefuseStorageTypeNotModelled)
	})
	t.Run("target-is-not-gp3", func(t *testing.T) {
		refuseWith(t, actFixture(t), actStep(
			actSpec(actEngine, StorageGP3, actSize, 12000, 1000),
			actSpec(actEngine, StorageGP2, actSize, 12000, 1000)), RefuseStorageTypeNotModelled)
	})
	t.Run("no-target-values", func(t *testing.T) {
		refuseWith(t, actFixture(t), actStep(from,
			actSpec(actEngine, StorageGP3, actSize, -1, -1)), RefuseBadStep)
	})
	t.Run("size-unusable", func(t *testing.T) {
		big := MaxParitySizeGiB + 1
		f := NewStorageActuateFixture(actClock(), actLive(func(r *InstanceStateRecord) {
			r.AllocatedStorageGiB = big
		}))
		f.WithEnvelope(actID, actGP3Envelope())
		f.WithEvents(actID)
		refuseWith(t, f, actStep(
			actSpec(actEngine, StorageGP2, big, -1, -1),
			actSpec(actEngine, StorageGP3, big, 12000, 1000)), RefuseSizeUnusable)
	})
}

// Every refusal code this unit adds is distinct from every other code in the
// package. Two codes with one value silently merge two findings in every
// roll-up, and an operator filtering on one of them then sees both.
func TestActuateReasonCodesAreDistinct(t *testing.T) {
	mine := map[string]string{
		"RefuseCooldownUnknown":     RefuseCooldownUnknown,
		"RefuseNotAvailable":        RefuseNotAvailable,
		"RefusePendingModification": RefusePendingModification,
		"RefuseBaselineArgument":    RefuseBaselineArgument,
		"RefuseRatchet":             RefuseRatchet,
		"RefuseAllocationDrift":     RefuseAllocationDrift,
		"RefuseWrongAction":         RefuseWrongAction,
		"RefuseBadStep":             RefuseBadStep,
		"RefuseNoChange":            RefuseNoChange,
		"RefuseEngineChanged":       RefuseEngineChanged,
		"RefuseGuardrailUnknown":    RefuseGuardrailUnknown,
		"RefuseInstanceMissing":     RefuseInstanceMissing,
		"RefuseDrift":               RefuseDrift,
	}
	existing := []string{
		ReasonParityStorageTypeNotModelled, ReasonParitySizeUnusable, ReasonParityGP2BandUnpublished,
		ReasonParityNotProvisionableBelowThreshold, ReasonParityEnvelopeUnknown, ReasonParityExceedsEnvelope,
		ReasonParityNoCheaperConfig, ReasonParityFloorsAtBaseline, ReasonParityNoMeasurement,
		ReasonParityWindowTooShort, ReasonParityStorageOptimization, ReasonParityCooldown,
		ReasonParityLowConfidence, ReasonAuroraNotSupported, ReasonClusterMemberNotSupported,
		ReasonModeOff, ReasonUnknownEngine, ReasonUnknownInstanceClass, ReasonEngineNotPriced,
		ReasonUnknownDeployment, ReasonUnverifiedRate, ReasonInstanceClassIsAFailover,
		ReasonFreeableMemoryIsPageCache, ReasonBufferPoolScalesWithClass, ReasonMemorySemanticsUnencoded,
		ReasonStorageCannotShrink, ReasonStorageAutoscalingRatchet, ReasonReplicaIsFailoverCapacity,
		ReasonMultiAZIsAvailabilityPosture, ReasonInsufficientWindow, ReasonNoMetricEvidence,
		ReasonTruncatedMetrics, ReasonSizeFlexibilityExcluded, ReasonInstanceStateUnstable,
		ReasonNoStoragePerformanceModel,
	}
	seen := map[string]string{}
	for _, c := range existing {
		seen[c] = "an existing U11/U13 code"
	}
	names := make([]string, 0, len(mine))
	for n := range mine {
		names = append(names, n)
	}
	sortStrings(names)
	for _, n := range names {
		c := mine[n]
		if c == "" {
			t.Errorf("%s is empty", n)
			continue
		}
		if prev, dup := seen[c]; dup {
			t.Errorf("%s = %q collides with %s", n, c, prev)
		}
		seen[c] = n
	}
	// The REUSED codes are reused on purpose and must NOT be new values.
	reused := map[string]string{
		"RefuseCooldown":               ReasonParityCooldown,
		"RefuseStateUnstable":          ReasonParityStorageOptimization,
		"RefuseEnvelopeUnknown":        ReasonParityEnvelopeUnknown,
		"RefuseExceedsEnvelope":        ReasonParityExceedsEnvelope,
		"RefuseNotProvisionable":       ReasonParityNotProvisionableBelowThreshold,
		"RefuseStorageTypeNotModelled": ReasonParityStorageTypeNotModelled,
		"RefuseSizeUnusable":           ReasonParitySizeUnusable,
		"RefuseUnknownEngine":          ReasonUnknownEngine,
		"RefuseModeOff":                ReasonModeOff,
	}
	for name, want := range reused {
		if want == "" {
			t.Errorf("%s reuses an empty code", name)
		}
	}
}
