package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agenticode/kilter/pkg/actuate"
	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/explain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
)

// These tests drive pkg/explain's REAL decomposition and REAL explain payload
// through the REAL CLI entry point, over recorded cluster snapshots. Both
// commands call Verify before printing, so a citation that does not resolve is
// a test failure rather than a rendered answer.

var whyCostT0 = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

// fleetSnapshot builds one priced fleet at an instant: n on-demand m5.large
// plus s spot m5.large, with one namespace's requested capacity.
func fleetSnapshot(at time.Time, onDemand, spot int, nsMilliCPU int64) *model.ClusterSnapshot {
	snap := &model.ClusterSnapshot{ClusterID: "why-cost-demo", Timestamp: at}
	add := func(n int, isSpot bool, prefix string) {
		for i := 0; i < n; i++ {
			snap.Nodes = append(snap.Nodes, model.NodeSpec{
				Name:        prefix + string(rune('a'+i)),
				Capacity:    model.Resources{MilliCPU: 2000, MemoryBytes: 8 << 30},
				Allocatable: model.Resources{MilliCPU: 1900, MemoryBytes: 7 << 30},
				Ready:       true, InstanceType: "m5.large", Spot: isSpot,
				Provider: "aws", Region: "us-east-1", Zone: "us-east-1a",
			})
		}
	}
	add(onDemand, false, "od-")
	add(spot, true, "spot-")

	ref := model.WorkloadRef{Kind: model.KindDeployment, Namespace: "shop", Name: "web"}
	snap.Pods = append(snap.Pods, model.PodSpec{
		UID: "pod-shop", Name: "web-0", Namespace: "shop", Workload: ref,
		NodeName: "od-a", Phase: "Running",
		Containers: []model.ContainerSpec{{
			Name:     "web",
			Requests: model.Resources{MilliCPU: nsMilliCPU, MemoryBytes: 1 << 30},
		}},
	})
	snap.Workloads = append(snap.Workloads, model.WorkloadInfo{Ref: ref, Replicas: 1, Ready: 1})
	return snap
}

func writeSnapshot(t *testing.T, dir, name string, snap *model.ClusterSnapshot) string {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// whyCostFleet writes a three-point history in which the fleet grows from four
// on-demand nodes to six nodes, two of which are spot: node count moves AND
// capacity type moves, which is exactly the overlap the attribution order
// exists to resolve.
func whyCostFleet(t *testing.T) (dir string, args []string) {
	t.Helper()
	dir = t.TempDir()
	var out []string
	for i, s := range []*model.ClusterSnapshot{
		fleetSnapshot(whyCostT0, 4, 0, 500),
		fleetSnapshot(whyCostT0.Add(12*time.Hour), 5, 1, 700),
		fleetSnapshot(whyCostT0.Add(24*time.Hour), 4, 2, 900),
	} {
		out = append(out, "--kube-snapshot",
			writeSnapshot(t, dir, "snap-"+string(rune('0'+i))+".json", s))
	}
	return dir, out
}

func runWhyCostOK(t *testing.T, args ...string) string {
	t.Helper()
	var b strings.Builder
	if err := runWhyCostTo(&b, args); err != nil {
		t.Fatalf("kilter why-cost: %v\n%s", err, b.String())
	}
	return b.String()
}

func whyCostAttribution(t *testing.T, args ...string) *explain.Attribution {
	t.Helper()
	raw := runWhyCostOK(t, append(args, "--json")...)
	var att explain.Attribution
	if err := json.Unmarshal([]byte(raw), &att); err != nil {
		t.Fatalf("decode attribution: %v\n%s", err, raw)
	}
	return &att
}

// TestWhyCostIsReachableAndAdditive.
//
// The invariant pkg/explain exists to protect, asserted at the CLI:
// sum(terms) + residual == delta, EXACTLY. Every amount is an int64 count of
// µUSD, so this is integer arithmetic and "exactly" is meant literally.
func TestWhyCostIsReachableAndAdditive(t *testing.T) {
	_, snaps := whyCostFleet(t)
	att := whyCostAttribution(t, append(snaps,
		"--from", whyCostT0.Format(time.RFC3339),
		"--to", whyCostT0.Add(24*time.Hour).Format(time.RFC3339))...)

	var sum explain.Micro
	for _, term := range att.Terms {
		sum += term.Micro
	}
	if sum+att.Residual.Micro != att.DeltaMicro {
		t.Errorf("sum(terms)=%d + residual=%d != delta=%d",
			sum, att.Residual.Micro, att.DeltaMicro)
	}
	if len(att.Residual.Evidence) == 0 {
		t.Error("the residual ships uncited")
	}
	if len(att.Terms) == 0 {
		t.Fatal("no terms; the decomposition explained nothing")
	}
	// Every term, sub-term and the residual must carry a citation. A number
	// the reader cannot trace is worse than a missing one.
	for _, term := range att.Terms {
		if len(term.Evidence) == 0 {
			t.Errorf("term %q ships uncited", term.Kind)
		}
		var subs explain.Micro
		for _, sub := range term.Of {
			subs += sub.Micro
			if len(sub.Evidence) == 0 {
				t.Errorf("sub-term %q of %q ships uncited", sub.Kind, term.Kind)
			}
		}
		if len(term.Of) > 0 && subs != term.Micro {
			t.Errorf("sum(%q.Of)=%d != %d", term.Kind, subs, term.Micro)
		}
	}
	if len(att.Order) == 0 {
		t.Error("the attribution does not state the convention it was computed under")
	}
}

// TestWhyCostNamesTheFactorsThatActuallyMoved. The fleet went from 4 on-demand
// nodes to 4 on-demand + 2 spot, so node count and spot ratio must both carry
// a non-zero term — otherwise the decomposition is arithmetically valid and
// tells the operator nothing.
func TestWhyCostNamesTheFactorsThatActuallyMoved(t *testing.T) {
	_, snaps := whyCostFleet(t)
	att := whyCostAttribution(t, append(snaps,
		"--from", whyCostT0.Format(time.RFC3339),
		"--to", whyCostT0.Add(24*time.Hour).Format(time.RFC3339))...)

	byKind := map[string]explain.Micro{}
	for _, term := range att.Terms {
		byKind[string(term.Kind)] = term.Micro
	}
	if byKind["node-count"] == 0 {
		t.Errorf("node count moved from 4 to 6 and the term is zero: %v", byKind)
	}
	if byKind["spot-ratio"] >= 0 {
		t.Errorf("two nodes moved to spot and the term is %d; want a saving", byKind["spot-ratio"])
	}
	if att.DeltaMicro == 0 {
		t.Error("the measured delta is zero; the fixture proves nothing")
	}
	// And the prose renders it rather than only the JSON.
	out := runWhyCostOK(t, append(snaps,
		"--from", whyCostT0.Format(time.RFC3339),
		"--to", whyCostT0.Add(24*time.Hour).Format(time.RFC3339))...)
	for _, want := range []string{"node-count", "spot-ratio"} {
		if !strings.Contains(out, want) {
			t.Errorf("the prose does not mention %q:\n%s", want, out)
		}
	}
}

// TestWhyCostRequiresItsWindow.
//
// pkg/explain has no clock on purpose: the window is an argument, so the same
// inputs give the same answer forever. A wall-clock default would make every
// stored answer unreplayable, so the CLI refuses rather than inventing one.
func TestWhyCostRequiresItsWindow(t *testing.T) {
	_, snaps := whyCostFleet(t)
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"no window", "--from and --to are required", snaps},
		{"only from", "--from and --to are required",
			append(append([]string{}, snaps...), "--from", whyCostT0.Format(time.RFC3339))},
		{"backwards", "--to must be after --from",
			append(append([]string{}, snaps...),
				"--from", whyCostT0.Add(24*time.Hour).Format(time.RFC3339),
				"--to", whyCostT0.Format(time.RFC3339))},
		{"one observation", "one timeline point is not a change",
			append(append([]string{}, snaps...),
				"--from", whyCostT0.Format(time.RFC3339),
				"--to", whyCostT0.Add(time.Hour).Format(time.RFC3339))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			err := runWhyCostTo(&b, tc.args)
			if err == nil {
				t.Fatalf("accepted:\n%s", b.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestWhyCostIsOrderAndRepeatIndependent: the answer must not depend on the
// order the snapshots were typed on the command line, nor on Go's map
// iteration order within a process.
func TestWhyCostIsOrderAndRepeatIndependent(t *testing.T) {
	dir, snaps := whyCostFleet(t)
	window := []string{
		"--from", whyCostT0.Format(time.RFC3339),
		"--to", whyCostT0.Add(24 * time.Hour).Format(time.RFC3339),
	}
	base := runWhyCostOK(t, append(append([]string{}, snaps...), window...)...)
	for i := 0; i < 6; i++ {
		if got := runWhyCostOK(t, append(append([]string{}, snaps...), window...)...); got != base {
			t.Fatalf("repeat %d differs in the same process", i)
		}
	}
	reversed := []string{
		"--kube-snapshot", filepath.Join(dir, "snap-2.json"),
		"--kube-snapshot", filepath.Join(dir, "snap-0.json"),
		"--kube-snapshot", filepath.Join(dir, "snap-1.json"),
	}
	if got := runWhyCostOK(t, append(reversed, window...)...); got != base {
		t.Error("the answer depends on the order snapshots were supplied in")
	}
}

// TestFargateIsExcludedFromTheCompositionAndLandsInTheResidual.
//
// A Fargate "node" is a single-pod VM billed per quantized pod, not a
// shareable machine, so pricing it per node shape would inflate the fleet
// total and put the error inside a TERM. Excluding it moves the gap to the
// residual, where it is visible — the correct behaviour, and a worse answer
// than pricing Fargate separately, which is why it is reported rather than
// absorbed.
func TestFargateIsExcludedFromTheCompositionAndLandsInTheResidual(t *testing.T) {
	dir := t.TempDir()
	withFargate := fleetSnapshot(whyCostT0, 4, 0, 500)
	withFargate.Nodes = append(withFargate.Nodes, model.NodeSpec{
		Name: "fargate-1", ManagedBy: model.ManagedByFargate,
		Labels:      map[string]string{model.LabelComputeType: "fargate"},
		Capacity:    model.Resources{MilliCPU: 96000, MemoryBytes: 384 << 30},
		Allocatable: model.Resources{MilliCPU: 96000, MemoryBytes: 384 << 30},
		Ready:       true, Provider: "aws", Region: "us-east-1", Zone: "us-east-1a",
	})
	withFargate.Pods = append(withFargate.Pods, model.PodSpec{
		UID: "pod-fg", Name: "job-0", Namespace: "batch", NodeName: "fargate-1",
		Workload: model.WorkloadRef{Kind: model.KindDeployment, Namespace: "batch", Name: "job"},
		Phase:    "Running",
		Containers: []model.ContainerSpec{{
			Name: "job", Requests: model.Resources{MilliCPU: 1000, MemoryBytes: 2 << 30},
		}},
		ProvisionedCapacity: model.Resources{MilliCPU: 1000, MemoryBytes: 2 * 1000 * 1000 * 1000},
	})
	a := writeSnapshot(t, dir, "a.json", withFargate)
	b := writeSnapshot(t, dir, "b.json", fleetSnapshot(whyCostT0.Add(24*time.Hour), 4, 0, 500))

	// The window is half-open, so --to must be strictly after the last
	// observation for that observation to be inside it.
	att := whyCostAttribution(t,
		"--kube-snapshot", a, "--kube-snapshot", b,
		"--from", whyCostT0.Format(time.RFC3339),
		"--to", whyCostT0.Add(25*time.Hour).Format(time.RFC3339))

	// The fleet of m5.large nodes did not move, so every priced term is zero
	// and the entire Fargate pod's cost is the residual — reported, never
	// absorbed into the biggest term.
	if att.Residual.Micro == 0 {
		t.Fatalf("the Fargate cost was absorbed into a term rather than reported: %+v", att.Terms)
	}
	for _, term := range att.Terms {
		if term.Kind == "node-count" && term.Micro != 0 {
			t.Errorf("node count did not move and the term is %d", term.Micro)
		}
	}
	if len(att.Notes) == 0 {
		t.Error("the residual is unexplained and unremarked")
	}
}

// TestWhyCostAttributesAppliedActionsAndIgnoresDryRuns.
//
// A dry-run moved no money. Counting one would attribute a cost change to a
// plan that changed nothing — the classic attribution lie with a plan
// attached — so the projection sets Applied only for an applied entry with
// confirmed steps, and only StatusDone steps are counted.
func TestWhyCostAttributesAppliedActionsAndIgnoresDryRuns(t *testing.T) {
	dir, snaps := whyCostFleet(t)
	del := func(node string) actuate.StepStatus {
		return actuate.StepStatus{
			Step:   plan.Step{Type: plan.StepDeleteNode, Node: node},
			Status: actuate.StatusDone,
		}
	}
	report := api.LedgerReport{Entries: []api.LedgerEntry{
		{
			At: whyCostT0.Add(6 * time.Hour), Cluster: "why-cost-demo", Mode: "apply",
			Fingerprint: "aaaa1111", Risk: "low", Done: 1,
			Steps: []actuate.StepStatus{del("od-d")},
		},
		{
			// A preview. It moved nothing and must be attributed nothing.
			At: whyCostT0.Add(8 * time.Hour), Cluster: "why-cost-demo", Mode: "dry-run",
			Fingerprint: "bbbb2222", Risk: "low", Done: 1,
			Steps: []actuate.StepStatus{
				{Step: plan.Step{Type: plan.StepDeleteNode, Node: "od-c"}, Status: actuate.StatusDryRun},
			},
		},
		{
			// Outside the window.
			At: whyCostT0.Add(-48 * time.Hour), Cluster: "why-cost-demo", Mode: "apply",
			Fingerprint: "cccc3333", Done: 1, Steps: []actuate.StepStatus{del("od-z")},
		},
	}}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(ledgerPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	actions, err := loadLedgerActions(ledgerPath, "why-cost-demo",
		whyCostT0, whyCostT0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("got %d actions in the window, want 2 (the third is outside it)", len(actions))
	}
	applied, dryRun := actions[0], actions[1]
	if !applied.Applied || applied.NodesRemoved != 1 {
		t.Errorf("the applied entry projected to %+v", applied)
	}
	if dryRun.Applied {
		t.Error("a dry-run was marked Applied; it moved no money")
	}
	if dryRun.NodesRemoved != 0 {
		t.Errorf("a dry-run step was counted as a node removal (%d)", dryRun.NodesRemoved)
	}
	if applied.NodesAdded != 0 || dryRun.NodesAdded != 0 {
		t.Error("NodesAdded must stay 0 until a plan type provisions nodes")
	}

	// End to end: the kilter-action sub-term appears under node-count.
	att := whyCostAttribution(t, append(append([]string{}, snaps...),
		"--ledger", ledgerPath,
		"--from", whyCostT0.Format(time.RFC3339),
		"--to", whyCostT0.Add(24*time.Hour).Format(time.RFC3339))...)
	var found bool
	for _, term := range att.Terms {
		for _, sub := range term.Of {
			if sub.Kind == "kilter-action" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no kilter-action sub-attribution: %+v", att.Terms)
	}
}

// TestExplainIsReachableAndCited.
//
// Every driver must be grounded in an evidence ID that resolves against the
// same store that produced the answer — §5.7's publish gate, which the CLI
// calls because nothing inside pkg/explain does it at serve time.
func TestExplainIsReachableAndCited(t *testing.T) {
	args := []string{
		"--kube-snapshot", readFixture(t, "cluster.json"),
		"--workload", "Deployment/default/api", "--container", "api",
	}
	var b strings.Builder
	if err := runExplainTo(&b, append(args, "--json")); err != nil {
		t.Fatalf("kilter explain: %v\n%s", err, b.String())
	}
	var payload explain.Explanation
	if err := json.Unmarshal([]byte(b.String()), &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, b.String())
	}
	if len(payload.Drivers) == 0 {
		t.Fatal("the explanation has no drivers")
	}
	for _, d := range payload.Drivers {
		if len(d.Evidence) == 0 {
			t.Errorf("driver %q ships ungrounded", d.Kind)
		}
	}
	if len(payload.Citations) == 0 {
		t.Error("the payload cites nothing")
	}
	// The prose form echoes the window it was computed over, so a stored
	// answer states its own window rather than depending on when it was read.
	out := run2(t, runExplainTo, args)
	if !strings.Contains(out, "over [") || !strings.Contains(out, "Z]") {
		t.Errorf("the prose does not echo the resolved window:\n%s", out)
	}
	if !strings.Contains(out, "usage-history") {
		t.Errorf("the prose does not render the drivers:\n%s", out)
	}
}

// TestExplainRefusesBadInput.
func TestExplainRefusesBadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no snapshot", []string{"--workload", "Deployment/a/b", "--container", "c"}, "required"},
		{"bad workload ref", []string{
			"--kube-snapshot", readFixture(t, "cluster.json"),
			"--workload", "api", "--container", "api"}, "Kind/namespace/name"},
		{"missing file", []string{
			"--kube-snapshot", "testdata/nope.json",
			"--workload", "Deployment/a/b", "--container", "c"}, "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			if err := runExplainTo(&b, tc.args); err == nil {
				t.Fatalf("accepted:\n%s", b.String())
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestSnapshotSeriesRejectsTiesAndMixedClusters: a tie has no defined replay
// order, and two clusters in one window is a different question.
func TestSnapshotSeriesRejectsTiesAndMixedClusters(t *testing.T) {
	dir := t.TempDir()
	a := writeSnapshot(t, dir, "a.json", fleetSnapshot(whyCostT0, 2, 0, 100))
	b := writeSnapshot(t, dir, "b.json", fleetSnapshot(whyCostT0, 3, 0, 100))
	if _, err := loadSnapshotSeries([]string{a, b}); err == nil ||
		!strings.Contains(err.Error(), "share the timestamp") {
		t.Errorf("err = %v, want a duplicate-timestamp error", err)
	}
	other := fleetSnapshot(whyCostT0.Add(time.Hour), 2, 0, 100)
	other.ClusterID = "somewhere-else"
	c := writeSnapshot(t, dir, "c.json", other)
	if _, err := loadSnapshotSeries([]string{a, c}); err == nil ||
		!strings.Contains(err.Error(), "different clusters") {
		t.Errorf("err = %v, want a mixed-cluster error", err)
	}
}

// run2 runs a writer-based command and fails the test on error.
func run2(t *testing.T, fn func(io.Writer, []string) error, args []string) string {
	t.Helper()
	var b strings.Builder
	if err := fn(&b, args); err != nil {
		t.Fatalf("command failed: %v\n%s", err, b.String())
	}
	return b.String()
}
