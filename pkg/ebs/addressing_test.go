package ebs

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/domain"
)

// snapshotOf runs the production collector over a fixture and returns the
// snapshot itself, so a test can choose the order snapshots are learned in.
// collectInto is the same thing with the order fixed at one.
func snapshotOf(t *testing.T, f *Fixture, now time.Time) *domain.Snapshot {
	t.Helper()
	c, err := NewCollector(f, f, CollectorConfig{Scope: "123456789012/us-east-1", Region: "us-east-1"})
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	snap, err := c.Collect(t.Context(), now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return snap
}

// siblingSnapshot is the instance half's snapshot as pkg/domain/ec2 ships it:
// the same Kind (the ec2 domain covers instances as well as volumes, see
// [Kind]), a Payload only that half can decode, and not one volume in it.
func siblingSnapshot(at time.Time, id string) *domain.Snapshot {
	return &domain.Snapshot{
		Domain:    Kind,
		Scope:     "123456789012/us-east-1",
		Timestamp: at,
		Payload: json.RawMessage(fmt.Sprintf(
			`{"scope":"123456789012/us-east-1","instances":[{"id":%q,"type":"m5.xlarge"}]}`, id)),
	}
}

// state is everything an observer downstream of this domain can see. Two
// domains fed the same snapshots must agree on all of it, whatever order the
// snapshots arrived in.
func stateOf(t *testing.T, d *Domain, now time.Time) string {
	t.Helper()
	rep := d.Assess(now, nil)
	steps, err := d.PlanSteps(d.Recommend(now, nil), domain.Guard{Now: now})
	plan := fmt.Sprintf("%d steps", len(steps))
	if err != nil {
		plan = "refused: " + err.Error()
	}
	return jsonOf(t, d.Health(now)) + "\n" + jsonOf(t, rep) + "\n" + plan
}

// learnAll folds snapshots into a fresh domain, in the order given.
func learnAll(t *testing.T, snaps ...*domain.Snapshot) *Domain {
	t.Helper()
	d := newDomain(t, testConfig())
	for i, s := range snaps {
		if err := d.Learn(s); err != nil {
			t.Fatalf("Learn(%d): %v", i, err)
		}
	}
	return d
}

// TestSiblingSnapshotDoesNotDecideHealth is cmd/FINDINGS.md §5.1, reproduced
// inside the package that owns the bug.
//
// Two collectors feed the `ec2` kind. Handed the instance half's snapshot,
// Learn saw zero volume targets and concluded ITS OWN collector had delivered
// no volumes — so whichever snapshot arrived last decided the domain's health,
// and the same two inputs in a different order produced a different health
// line and a different plan refusal.
func TestSiblingSnapshotDoesNotDecideHealth(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	f := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)},
		measured("vol-a", base, 20, 4000, 100))
	vols := snapshotOf(t, f, now)
	inst := siblingSnapshot(now.Add(time.Minute), "i-0abc")

	at := now.Add(2 * time.Minute)
	volumesFirst := stateOf(t, learnAll(t, vols, inst), at)
	instancesFirst := stateOf(t, learnAll(t, inst, vols), at)
	if volumesFirst != instancesFirst {
		t.Fatalf("the same two snapshots in a different order produced different answers\n"+
			"volumes first:\n%s\n\ninstances first:\n%s", volumesFirst, instancesFirst)
	}

	// Pin which of the two answers is right: the sibling's snapshot is not
	// evidence about this collector, so the domain is exactly as healthy as
	// its own clean collection left it.
	only := stateOf(t, learnAll(t, vols), at)
	if volumesFirst != only {
		t.Errorf("the sibling's snapshot changed this domain's answer\n"+
			"with it:\n%s\n\nwithout it:\n%s", volumesFirst, only)
	}
	if h := learnAll(t, vols, inst).Health(at); !h.Ready || h.Reason != "" {
		t.Errorf("a clean collection plus the sibling's snapshot reports %+v, want ready", h)
	}
}

// TestEmptyAccountStaysDistinguishableFromNoCollection is the other half of
// the fix, and the reason it cannot simply ignore every volume-less snapshot.
// "We looked and found none" is a real answer about a real (empty) account;
// "we never looked" is a broken collector. Collapsing the two would be worse
// than the bug.
func TestEmptyAccountStaysDistinguishableFromNoCollection(t *testing.T) {
	now := base.Add(48 * time.Hour)
	at := now.Add(time.Minute)

	// Never looked: nothing was ever learned.
	never := newDomain(t, testConfig())
	if h := never.Health(at); h.Ready || h.Reason == "" {
		t.Errorf("a domain that learned nothing reports %+v", h)
	}

	// Looked, found none: this domain's own collector reported an empty
	// account. That must still degrade the domain and say why — it is a
	// snapshot addressed to us.
	empty := learnAll(t, &domain.Snapshot{Domain: Kind, Scope: "123456789012/us-east-1", Timestamp: now})
	h := empty.Health(at)
	if h.Ready {
		t.Errorf("an empty EBS collection left the domain ready: %+v", h)
	}
	if h.Reason == "" {
		t.Error("an empty EBS collection degraded the domain without saying why")
	}

	// And the sibling's snapshot must not be able to fake either state.
	sib := learnAll(t, siblingSnapshot(now, "i-0abc"))
	if got := sib.Health(at); got != never.Health(at) {
		t.Errorf("the sibling's snapshot moved a domain that never collected:\n got %+v\nwant %+v",
			got, never.Health(at))
	}
}

// TestSiblingSnapshotsAreInertUnderShuffle hardens the ordering property past
// the two-snapshot case: any interleaving of the sibling half's snapshots with
// this half's must produce exactly the answer this half's snapshot produces
// alone. Snapshots that say nothing about us must commute with everything.
func TestSiblingSnapshotsAreInertUnderShuffle(t *testing.T) {
	now := base.Add(48 * time.Hour)
	clock := newClock(now)
	f := newFixture(clock,
		[]VolumeRecord{gp2Volume("vol-a", 4000), gp2Volume("vol-b", 350), gp3Volume("vol-c", 500, 3000, 125)},
		append(measured("vol-a", base, 20, 4000, 100), measured("vol-b", base, 20, 900, 30)...))
	vols := snapshotOf(t, f, now)

	// Six sibling snapshots, spread either side of the volume snapshot in
	// time, one of them a partial collection: none of it is our evidence.
	siblings := make([]*domain.Snapshot, 0, 6)
	for i := 0; i < 6; i++ {
		s := siblingSnapshot(now.Add(time.Duration(i-3)*time.Hour), fmt.Sprintf("i-%04d", i))
		if i == 2 {
			s.Stale, s.StaleReason = true, "us-east-1c timed out"
		}
		siblings = append(siblings, s)
	}

	at := now.Add(2 * time.Hour)
	want := stateOf(t, learnAll(t, vols), at)

	rng := rand.New(rand.NewSource(11))
	for round := 0; round < 24; round++ {
		order := append([]*domain.Snapshot{vols}, siblings...)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		if got := stateOf(t, learnAll(t, order...), at); got != want {
			t.Fatalf("round %d: arrival order changed the answer\n got:\n%s\n\nwant:\n%s", round, got, want)
		}
	}
}

// FuzzSnapshotArrivalOrder walks arbitrary permutations rather than the
// seeded ones, and adds the degenerate shapes a shuffle test would not think
// to build: a nil snapshot, an empty one, an instance-only snapshot with no
// payload at all.
func FuzzSnapshotArrivalOrder(f *testing.F) {
	f.Add(uint64(0))
	f.Add(uint64(1))
	f.Add(uint64(0x9e3779b97f4a7c15))
	f.Fuzz(func(t *testing.T, seed uint64) {
		now := base.Add(48 * time.Hour)
		clock := newClock(now)
		fx := newFixture(clock, []VolumeRecord{gp2Volume("vol-a", 4000)},
			measured("vol-a", base, 20, 4000, 100))
		vols := snapshotOf(t, fx, now)

		inert := []*domain.Snapshot{
			nil,
			siblingSnapshot(now.Add(-time.Hour), "i-0abc"),
			siblingSnapshot(now.Add(time.Hour), "i-0def"),
			// An instance-only inventory with no payload: still not one word
			// about which volumes exist.
			{Domain: Kind, Scope: "123456789012/us-east-1", Timestamp: now.Add(time.Minute),
				Targets: []domain.Target{{
					Ref:  domain.TargetRef{Domain: Kind, Scope: "123456789012/us-east-1", ID: "i-0abc"},
					Spec: domain.Spec{Attrs: map[string]string{"instanceType": "m5.xlarge"}},
				}}},
		}

		order := append([]*domain.Snapshot{vols}, inert...)
		rng := rand.New(rand.NewSource(int64(seed)))
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

		at := now.Add(2 * time.Hour)
		want := stateOf(t, learnAll(t, vols), at)
		if got := stateOf(t, learnAll(t, order...), at); got != want {
			t.Fatalf("seed %d: arrival order changed the answer\n got:\n%s\n\nwant:\n%s", seed, got, want)
		}
	})
}
