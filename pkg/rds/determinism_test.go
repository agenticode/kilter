package rds

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/agenticode/kilter/pkg/domain"
)

// Determinism. Two collectors that walked the same account in a different
// order must ship the same snapshot, and two sizers over the same snapshot
// must render the same bytes — down to the last float64 addition.
//
// The permutations below are FIXED rather than randomly generated: a
// shuffle-invariance test seeded from the clock reports a different failure
// every run and is therefore unactionable.

// permutations of eight elements, chosen to include the identity, the
// reversal, an odd/even interleave and a rotation.
func permutations(n int) [][]int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	rev := make([]int, n)
	for i := range rev {
		rev[i] = n - 1 - i
	}
	inter := make([]int, 0, n)
	for i := 0; i < n; i += 2 {
		inter = append(inter, i)
	}
	for i := 1; i < n; i += 2 {
		inter = append(inter, i)
	}
	rot := make([]int, 0, n)
	for i := 0; i < n; i++ {
		rot = append(rot, (i+3)%n)
	}
	return [][]int{idx, rev, inter, rot}
}

func permute[T any](in []T, order []int) []T {
	out := make([]T, 0, len(in))
	for _, i := range order {
		out = append(out, in[i])
	}
	return out
}

// The account used by the determinism tests: eight instances covering every
// path the sizer has — priced and unpriced, excluded and modelled, idle and
// busy, Single-AZ and Multi-AZ, replica and primary, autoscaling and fixed.
func mixedFixture() *Fixture {
	instances := []DBInstanceRecord{
		rec("alpha", "db.r6i.xlarge", "postgres", withStorage(4096, 0, StorageGP2)),
		rec("bravo", "db.m6i.large", "mysql", withMultiAZ(), withStorage(200, 1000, StorageGP2)),
		rec("charlie", "db.t4g.medium", "mariadb"),
		rec("delta", "db.r6i.large", "aurora-mysql", withCluster("cl-1")),
		rec("echo", "db.r6i.large", "sqlserver-se", withLicense(LicenseIncluded)),
		rec("foxtrot", "db.r6i.xlarge", "postgres", withReplicaOf("alpha")),
		rec("golf", "db.r6i.large", "postgres", withTags(map[string]string{TagKilterMode: "off"})),
		rec("hotel", "db.m5.2xlarge", "postgres", withStatus(StatusModifying)),
	}
	metrics := mergeMetrics(
		metricsFor("alpha", 35, 20, 24<<30, 3800*GiB),
		metricsFor("bravo", 12, 3, 4<<30, 150*GiB),
		metricsFor("charlie", 2, 0, 2<<30, 80*GiB),
		metricsFor("delta", 40, 15, 8<<30, 40*GiB),
		metricsFor("echo", 25, 8, 8<<30, 40*GiB),
		metricsFor("foxtrot", 1, 0, 24<<30, 40*GiB),
		metricsFor("golf", 30, 10, 8<<30, 40*GiB),
		metricsFor("hotel", 60, 40, 12<<30, 40*GiB),
	)
	return &Fixture{
		Instances: instances,
		Clusters:  []DBClusterRecord{{DBClusterIdentifier: "cl-1", Engine: "aurora-mysql"}},
		Metrics:   metrics,
		Reservations: []ReservedDBInstanceRecord{
			{ReservedDBInstanceId: "r-1", DBInstanceClass: "db.r6i.xlarge", DBInstanceCount: 1,
				ProductDescription: "postgresql", State: "active", UsagePrice: 0.30,
				Duration: 365 * 24 * 3600, StartTime: testStart},
			{ReservedDBInstanceId: "r-2", DBInstanceClass: "db.m6i.large", DBInstanceCount: 2,
				ProductDescription: "mysql", State: "active", UsagePrice: 0.10, MultiAZ: true,
				Duration: 3 * 365 * 24 * 3600, StartTime: testStart},
		},
	}
}

// TestReportIsShuffleInvariant is the acceptance test the design doc names.
// It shuffles every input a collector could see in a different order — the
// instance pages, the cluster pages, the reservation pages, and the datapoint
// order inside each metric — and asserts the rendered report is byte-identical.
func TestReportIsShuffleInvariant(t *testing.T) {
	base := mixedFixture()
	var want []byte

	for pi, order := range permutations(len(base.Instances)) {
		f := &Fixture{
			Instances:    permute(base.Instances, order),
			Clusters:     base.Clusters,
			Metrics:      reverseEveryMetric(base.Metrics, pi%2 == 1),
			Reservations: permute(base.Reservations, []int{(pi) % 2, (pi + 1) % 2}),
			PageSize:     1 + pi, // and a different pagination boundary each time
		}
		led := ledgerWith(collect(t, f).Reservations)
		rep := assess(t, collect(t, f), led)

		var buf bytes.Buffer
		if err := rep.WriteText(&buf); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		j, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		got := append(buf.Bytes(), j...)
		if want == nil {
			want = got
			continue
		}
		if !bytes.Equal(want, got) {
			t.Fatalf("permutation %d rendered a different report; input ORDER reached the output.\n"+
				"first %d bytes, this %d bytes", pi, len(want), len(got))
		}
	}
	if want == nil {
		t.Fatal("no permutation ran")
	}
}

// reverseEveryMetric optionally hands each series to the collector in
// descending timestamp order, which is a thing CloudWatch is free to do.
func reverseEveryMetric(in map[string][]Point, do bool) map[string][]Point {
	out := make(map[string][]Point, len(in))
	for k, v := range in {
		if !do {
			out[k] = v
			continue
		}
		r := make([]Point, 0, len(v))
		for i := len(v) - 1; i >= 0; i-- {
			r = append(r, v[i])
		}
		out[k] = r
	}
	return out
}

// A checkpoint round-trips exactly, and the restored domain renders the same
// report. Without that, a report cannot be compared against yesterday's.
func TestCheckpointRoundTripsDeterministically(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	snap := collect(t, mixedFixture())
	if err := d.Observe(snap); err != nil {
		t.Fatal(err)
	}
	first := d.Report(testNow, nil)

	b1, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := d.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatal("two Checkpoints of the same state differ")
	}

	restored, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(b1); err != nil {
		t.Fatal(err)
	}
	b3, err := restored.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b3) {
		t.Fatal("Checkpoint → Restore → Checkpoint is not a fixed point")
	}
	second := restored.Report(testNow, nil)
	j1, _ := json.Marshal(first)
	j2, _ := json.Marshal(second)
	if !bytes.Equal(j1, j2) {
		t.Fatal("the restored domain renders a different report")
	}

	// An empty, a nil and a garbage checkpoint are handled without panic.
	if err := restored.Restore(nil); err != nil {
		t.Errorf("Restore(nil): %v", err)
	}
	if err := restored.Restore([]byte("{")); err == nil {
		t.Error("Restore accepted malformed JSON")
	}
	if err := restored.Restore([]byte(`{"version":99}`)); err == nil {
		t.Error("Restore accepted an unknown checkpoint version")
	}
}

// Observe carries the previous allocated storage forward, which is what gives
// trap 8's ledger rule something to compare against.
func TestObserveRemembersThePriorStorageFloor(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{
		Instances: []DBInstanceRecord{rec("grow", "db.m6i.large", "postgres", withStorage(200, 1000, StorageGP2))},
		Metrics:   metricsFor("grow", 20, 5, 6<<30, 20*GiB),
	}
	if err := d.Observe(collect(t, f)); err != nil {
		t.Fatal(err)
	}
	// Autoscaling fires between the two runs. Nothing Kilter did; nothing
	// CloudTrail recorded.
	f.Instances[0].AllocatedStorage = 240
	if err := d.Observe(collect(t, f)); err != nil {
		t.Fatal(err)
	}
	rep := d.Report(testNow, nil)
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
	a := must(t, rep, "grow")
	if a.PriorAllocatedStorageGiB != 200 {
		t.Fatalf("prior allocated storage = %d, want 200", a.PriorAllocatedStorageGiB)
	}
	var saw string
	for _, ev := range a.Evidence {
		if ev.Metric == "allocated-storage-change" {
			saw = ev.Value
		}
	}
	if saw == "" {
		t.Fatal("a storage increase between snapshots produced no evidence line")
	}
	if want := string(StorageGrewUnattributed); saw[:len(want)] != want {
		t.Errorf("the growth was attributed as %q, want %q", saw, want)
	}
}

// The generic seam round-trips losslessly through Payload, and lossily — but
// honestly — without it.
func TestGenericSnapshotIsLosslessThroughPayload(t *testing.T) {
	snap := collect(t, mixedFixture())
	g := snap.Generic()
	if g == nil || len(g.Payload) == 0 {
		t.Fatal("Generic() carried no payload; the lossy path would be the only one")
	}
	if g.Commitments == nil || len(g.Commitments.ReservedDBs) != len(snap.Reservations) {
		t.Fatal("Generic() dropped the reservation inventory")
	}
	for _, tgt := range g.Targets {
		if len(tgt.Blind) == 0 {
			t.Errorf("%s declares no blind spots on the lossy path", tgt.Ref.ID)
		}
	}

	// Payload path: identical report.
	viaPayload, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := viaPayload.Learn(g); err != nil {
		t.Fatal(err)
	}
	direct, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Observe(snap); err != nil {
		t.Fatal(err)
	}
	j1, _ := json.Marshal(viaPayload.Report(testNow, nil))
	j2, _ := json.Marshal(direct.Report(testNow, nil))
	if !bytes.Equal(j1, j2) {
		t.Fatal("Learn(Payload) and Observe produced different reports; the payload path is lossy")
	}

	// Samples-only path: every series is marked partial, so no idle verdict
	// can fire on evidence that cannot say whether it is complete.
	lossy := *g
	lossy.Payload = nil
	viaSamples, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := viaSamples.Learn(&lossy); err != nil {
		t.Fatal(err)
	}
	rep := viaSamples.Report(testNow, nil)
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, a := range rep.Assessments {
		if a.Idle.Known {
			t.Errorf("%s produced an idle verdict from samples that cannot report truncation", a.Target.ID)
		}
	}
}

// A snapshot addressed to another domain is a wiring bug and is reported as
// one; everything else degrades.
func TestLearnRejectsOnlyAForeignSnapshot(t *testing.T) {
	d, err := NewDomain(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Learn(nil); err != nil {
		t.Errorf("Learn(nil): %v", err)
	}
	if err := d.Observe(nil); err != nil {
		t.Errorf("Observe(nil): %v", err)
	}
	if err := d.Learn(&domain.Snapshot{Domain: domain.EC2}); err == nil {
		t.Error("Learn accepted a snapshot addressed to another domain")
	}
	if err := d.Observe(&Snapshot{Domain: "ec2"}); err == nil {
		t.Error("Observe accepted a snapshot addressed to another domain")
	}
	// An empty snapshot for THIS domain degrades rather than failing.
	if err := d.Learn(&domain.Snapshot{Domain: Kind}); err != nil {
		t.Errorf("an empty snapshot for this domain must degrade, not fail: %v", err)
	}
	if h := d.Health(testNow); h.Ready {
		t.Error("a domain with no targets reported itself ready")
	}
}
