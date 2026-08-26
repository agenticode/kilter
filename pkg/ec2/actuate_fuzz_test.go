package ec2

// The invariant this whole unit exists to protect, fuzzed.
//
//	An EC2 instance is never left stopped and forgotten.
//
// Formally, after ANY execution — however it failed, at whatever stage, with
// whatever combination of failed calls and lost responses — one of these holds:
//
//	(a) the instance is running, or
//	(b) the actuator holds a NON-TERMINAL ledger entry for the step, so a
//	    later run re-observes the account and drives it home.
//
// A terminal entry with a stopped instance is the failure: it means every
// future run skips the step and nobody ever starts the instance again. That is
// the outage nobody is looking for, and it is what this target hunts.
//
// The second half of the target is liveness: once the injected faults stop, a
// fixed number of further executions must actually get the instance running.
// An actuator that satisfied (b) by failing forever would satisfy the safety
// property and be useless.

import (
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

func FuzzStopStartNeverStrandsAnInstance(f *testing.F) {
	f.Add([]byte{0}, uint8(0))
	f.Add([]byte{1, 0, 0, 0, 0}, uint8(1))
	f.Add([]byte{0, 0, 5, 0, 7, 0, 0}, uint8(2))
	f.Add([]byte{5, 7, 5, 7, 5, 7}, uint8(0))
	f.Add([]byte{3, 1, 4, 1, 5, 9, 2, 6}, uint8(3))
	f.Add([]byte{255, 0, 128, 64, 32, 16}, uint8(1))

	f.Fuzz(func(t *testing.T, script []byte, settle uint8) {
		if len(script) == 0 {
			script = []byte{0}
		}
		clock := newActClock(actBase)
		fix := newActFixture(clock)
		fix.SettleAfter = int(settle % 4)

		// The script drives fault injection deterministically. Each consulted
		// operation takes the next byte; a byte divisible by 5 fails the call
		// before its effect, one divisible by 7 fails it after — the lost
		// response, which is the case that makes the account and the
		// controller disagree.
		cursor := 0
		nextByte := func() byte {
			b := script[cursor%len(script)]
			cursor++
			return b
		}
		faulty := map[string]bool{
			OpDescribeInstance: true, OpStopInstances: true,
			OpStartInstances: true, OpModifyInstanceAttribute: true,
		}
		fix.Fail = func(op string, _ int) error {
			if faulty[op] && nextByte()%5 == 0 {
				return errInjected
			}
			return nil
		}
		fix.FailAfter = func(op string, _ int) error {
			if faulty[op] && nextByte()%7 == 0 {
				return errInjected
			}
			return nil
		}

		step := actStep(actStepOpts{})
		ap, err := NewApproval([]domain.Step{step}, actToken([]domain.Step{step}, actBase), actBase)
		if err != nil {
			t.Fatalf("NewApproval: %v", err)
		}
		as, err := ap.Authorize(step)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}

		// Phase 1: chaos. Each attempt is a fresh controller with no ledger —
		// the harshest restart there is.
		for attempt := 0; attempt < 8; attempt++ {
			a := newActActuator(t, fix, clock, ModeApply, func(c *ActuatorConfig) {
				c.PollInterval = time.Second
				c.PollTimeout = 4 * time.Second
			})
			_ = a.Execute(t.Context(), as)

			live, ok := fix.Instance("i-app")
			if !ok {
				t.Fatal("the instance vanished; this unit never terminates anything")
			}
			if live.State == StateRunning {
				continue
			}
			e, ok := a.Entry(step.Key)
			if !ok {
				t.Fatalf("attempt %d: instance is %s with no ledger entry — stopped and forgotten", attempt, live.State)
			}
			if e.Terminal() {
				t.Fatalf("attempt %d: instance is %s/%s but its ledger entry is terminal (%q) — stopped and forgotten",
					attempt, live.InstanceType, live.State, e.Status)
			}
			if e.Settled() {
				t.Fatalf("attempt %d: instance is %s but its entry claims it is at rest (%q)",
					attempt, live.State, e.Status)
			}
		}

		// Phase 2: liveness. With the faults gone, the machine must resolve.
		fix.Fail, fix.FailAfter = nil, nil
		fix.SettleAfter = 0
		for attempt := 0; attempt < 6; attempt++ {
			a := newActActuator(t, fix, clock, ModeApply, nil)
			if err := a.Execute(t.Context(), as); err == nil {
				break
			}
		}
		live, _ := fix.Instance("i-app")
		if live.State != StateRunning {
			t.Fatalf("with no faults left, the instance is still %s/%s", live.InstanceType, live.State)
		}
		// The type is whichever the machine settled on — the target, or the
		// original after a rollback. Both are safe; a stopped instance is not.
		if live.InstanceType != "m6i.2xlarge" && live.InstanceType != "m5.2xlarge" {
			t.Fatalf("the instance ended as %s, which is neither the plan's From nor its To", live.InstanceType)
		}
	})
}

// The decision layer, fuzzed on its own: no attribute soup may talk the
// pre-flight into approving a step that cuts memory without a signal, spends
// money, or shrinks storage. This runs with no cloud seam reachable at all.
func FuzzPreflightNeverApprovesAForbiddenChange(f *testing.F) {
	f.Add("m6i.2xlarge", int64(8000), int64(32), "cwagent", "42.5", "61", int64(100), int64(100))
	f.Add("m6i.xlarge", int64(4000), int64(16), "none", "10", "10", int64(100), int64(100))
	f.Add("m6i.xlarge", int64(4000), int64(16), "cwagent", "-5", "1", int64(100), int64(50))
	f.Add("", int64(0), int64(0), "", "", "", int64(0), int64(0))

	f.Fuzz(func(t *testing.T, toType string, toCPU, toMemGiB int64, signal, net, gross string, fromStor, toStor int64) {
		if toMemGiB < 0 || toMemGiB > 1<<20 {
			t.Skip()
		}
		clock := newActClock(actBase)
		fix := newActFixture(clock)
		a := newActActuator(t, fix, clock, ModeApply, nil)

		step := actStep(actStepOpts{
			toType: toType, toCPU: toCPU, toMem: toMemGiB << 30,
			memSig: signal, net: net, gross: gross,
			fromStor: itoa64(fromStor), toStor: itoa64(toStor),
		})
		err := a.Preflight(t.Context(), step)
		if err != nil {
			// A refusal must always be legible: a code and prose.
			if code := RefusalCode(err); code == "" && IsRefusal(err) {
				t.Fatalf("a refusal carried no code: %v", err)
			}
			return
		}
		// It passed. Then every forbidden shape must be absent, checked
		// independently of the code that just approved it.
		in, derr := decodeStep(step, domain.ActionStopStart)
		if derr != nil {
			t.Fatalf("a step that passed pre-flight does not decode: %v", derr)
		}
		if in.toMem < in.fromMem && in.memorySignal != MemorySignalCWAgent {
			t.Fatalf("approved a memory cut %d → %d with signal %q (§7 trap 4)",
				in.fromMem, in.toMem, in.memorySignal)
		}
		if in.net <= 0 {
			t.Fatalf("approved a change claiming net %v (§7 trap 1)", in.net)
		}
		if in.net > in.gross+1e-9 {
			t.Fatalf("approved net %v above gross %v", in.net, in.gross)
		}
		if in.toStor < in.fromStor {
			t.Fatalf("approved a storage reduction %d → %d GiB", in.fromStor, in.toStor)
		}
		if in.checkedAt.IsZero() {
			t.Fatal("approved a change with no commitment check")
		}
		if fix.Mutations() != 0 {
			t.Fatal("Preflight mutated the account")
		}
	})
}
