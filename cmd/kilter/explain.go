package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/actuate"
	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/explain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
)

// `kilter explain` and `kilter why-cost` — the explanation plane, made
// runnable.
//
// Both commands read a SEQUENCE of recorded cluster snapshots, the format
// `kilter analyze --dump-snapshot` already writes. That is the honest input:
// pkg/explain has no clock and no network, its window is an argument, and both
// entry points need history that pkg/store does not keep (see
// cmd/WIRING-FINDINGS.md). Feeding them recorded snapshots is the same
// discipline `kilter domains` and `kilter simulate` already use.
//
// # The publish gate is not optional
//
// pkg/explain's central rule is §5.6/§5.7's: a number the reader cannot trace
// is worse than a missing one, so every Term, every Driver and the residual
// carry an evidence ID, and every ID must resolve against the same store that
// produced the answer. Nothing inside pkg/explain enforces that at serve time
// — Verify exists and someone has to call it. These commands call it before
// printing, and treat a failure as an error rather than rendering an answer
// with a dangling citation.

const explainUsage = `kilter explain — why the engine would resize this container

Usage:
  kilter explain --kube-snapshot PATH [--kube-snapshot PATH ...] \
                 --workload Kind/namespace/name --container NAME [flags]

Snapshots are replayed in timestamp order through the real recommender, so at
least two are needed before anything can be said. Nothing here calls a cluster.

Flags:
  --kube-snapshot PATH   cluster snapshot JSON (repeatable, any order)
  --workload REF         Kind/namespace/name, e.g. Deployment/default/api
  --container NAME       container within the workload
  --from RFC3339         evidence window start (default: the first snapshot)
  --to RFC3339           evidence window end   (default: the last snapshot)
  --catalog PATH         pricing catalog JSON (default: embedded)
  --json                 emit the payload instead of the prose
`

const whyCostUsage = `kilter why-cost — an additive, individually-citable cost decomposition

Usage:
  kilter why-cost --kube-snapshot PATH [--kube-snapshot PATH ...] \
                  --from RFC3339 --to RFC3339 [flags]

--from and --to are REQUIRED. There is no default window: the window is an
argument, and a wall-clock default makes a stored answer unreplayable.

The decomposition is over the HOURLY RUN RATE, not integrated spend, and it
satisfies sum(terms) + residual == delta exactly — every amount is an int64
count of millionths of a dollar, so the sum is order-independent by
construction.

Flags:
  --kube-snapshot PATH   cluster snapshot JSON (repeatable, any order)
  --from RFC3339         window start, inclusive (required)
  --to RFC3339           window end, EXCLUSIVE (required) — [from, to), matching
                         pkg/evidence's window convention
  --ledger PATH          kilter ledger --json output, for the kilter-action term
  --catalog PATH         pricing catalog JSON (default: embedded)
  --json                 emit the attribution instead of the prose
`

func runExplain(args []string) error { return runExplainTo(os.Stdout, args) }
func runWhyCost(args []string) error { return runWhyCostTo(os.Stdout, args) }

// ---------------------------------------------------------------- why-cost

func runWhyCostTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("why-cost", flag.ContinueOnError)
	fs.SetOutput(w)
	var snaps repeatedFlag
	fs.Var(&snaps, "kube-snapshot", "cluster snapshot JSON (repeatable)")
	from := fs.String("from", "", "window start (RFC3339, required)")
	to := fs.String("to", "", "window end (RFC3339, required)")
	ledgerPath := fs.String("ledger", "", "kilter ledger --json output")
	catalogPath := fs.String("catalog", "", "pricing catalog JSON")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(snaps) == 0 {
		fmt.Fprint(w, whyCostUsage)
		return fmt.Errorf("why-cost: at least one --kube-snapshot is required")
	}
	if *from == "" || *to == "" {
		fmt.Fprint(w, whyCostUsage)
		return fmt.Errorf("why-cost: --from and --to are required; the window is an argument, " +
			"and an answer computed over a wall-clock default cannot be replayed")
	}
	start, err := time.Parse(time.RFC3339, *from)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	end, err := time.Parse(time.RFC3339, *to)
	if err != nil {
		return fmt.Errorf("--to: %w", err)
	}
	if !end.After(start) {
		return fmt.Errorf("why-cost: --to must be after --from")
	}
	catalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	series, err := loadSnapshotSeries(snaps)
	if err != nil {
		return err
	}
	cluster := series[0].ClusterID

	store, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		return err
	}
	// Observe a timeline point per snapshot in the window. The point's cost is
	// pkg/pricing's own SnapshotCost, which includes Fargate; the composition
	// below excludes Fargate nodes, so the difference lands in the residual
	// with a note. That gap is honest and coarse, and naming it beats hiding
	// it inside a term.
	// [from, to) — HALF-OPEN, matching pkg/evidence's own window convention
	// and the filter explain.WhyCost applies internally. Observing on a
	// different convention than the consumer filters on would record a point
	// the decomposition then ignores, and the count check below would pass
	// while WhyCost failed.
	var inWindow []*model.ClusterSnapshot
	for _, s := range series {
		if s.Timestamp.Before(start) || !s.Timestamp.Before(end) {
			continue
		}
		cost := catalog.SnapshotCost(s)
		if err := store.ObservePoint(cluster, evidence.TimelinePoint{
			At: s.Timestamp, CostUSDPerHour: cost.HourlyUSD, Nodes: countPricedNodes(s),
		}); err != nil {
			return fmt.Errorf("why-cost: record timeline: %w", err)
		}
		inWindow = append(inWindow, s)
	}
	observed := len(inWindow)
	if observed < 2 {
		return fmt.Errorf("why-cost: %d snapshot(s) inside [%s, %s) — a change needs two "+
			"observations, and one timeline point is not a change. Note the window is "+
			"half-open: a snapshot at exactly --to is outside it",
			observed, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	}
	timeline, err := store.Timeline(cluster, start, end)
	if err != nil {
		return err
	}

	actions, err := loadLedgerActions(*ledgerPath, cluster, start, end)
	if err != nil {
		return err
	}

	// The two edges are the FIRST and LAST snapshots that produced a timeline
	// point in the window, not the newest snapshot on either side of the
	// requested boundary. They must be the same two instants the measured
	// ΔCost was taken between: a composition describing t+24h against a
	// measurement taken at t+12h would push a real, explainable fleet change
	// into the residual and call it unexplained.
	att, err := explain.WhyCost(explain.Input{
		Cluster: cluster, From: start, To: end,
		Timeline: timeline,
		Start:    basisFrom(inWindow[0], catalog),
		End:      basisFrom(inWindow[observed-1], catalog),
		Actions:  actions,
	})
	if err != nil {
		// A decomposition that cannot be computed is an error, not an empty
		// table.
		return fmt.Errorf("why-cost: %w", err)
	}
	// §5.7's publish gate. Nothing else enforces it.
	if err := att.Verify(explain.Resolver{Store: store, Actions: actions}); err != nil {
		return fmt.Errorf("why-cost: the answer has a citation that does not resolve, so it is not "+
			"publishable: %w", err)
	}
	if *jsonOut {
		return writeJSON(w, att)
	}
	_, err = io.WriteString(w, att.Prose()+"\n")
	return err
}

// basisFrom prices one snapshot's fleet composition.
//
// Three things pkg/explain/FINDINGS.md says the wiring must get right, all
// here:
//
//  1. FARGATE NODES ARE EXCLUDED. A Fargate "node" is a single-pod VM billed
//     per quantized pod, not a shareable machine; pricing it per node shape
//     would inflate the fleet total and put the error inside a term instead of
//     in the residual.
//  2. An empty fleet is &CostBasis{At: t}, NOT nil. nil means "I could not
//     determine the composition"; a cluster created inside the window really
//     was empty at the start edge, and conflating the two silently downgrades
//     a complete answer into a residual.
//  3. Namespace demand is REQUESTED capacity, not usage — requests are what
//     force node count. The clamping rule is pkg/plan's clampedRequests
//     (negatives to zero), duplicated rather than re-invented because that
//     function is unexported and pkg/plan is not this unit's to change.
func basisFrom(snap *model.ClusterSnapshot, cat *pricing.Catalog) *explain.CostBasis {
	if snap == nil {
		return nil
	}
	b := &explain.CostBasis{At: snap.Timestamp}

	type groupKey struct {
		instanceType string
		spot         bool
	}
	groups := map[groupKey]*explain.NodeGroup{}
	for i := range snap.Nodes {
		n := &snap.Nodes[i]
		if n.IsFargate() {
			continue
		}
		hourly, _ := cat.NodeHourlyCost(n)
		k := groupKey{n.InstanceType, n.Spot}
		g := groups[k]
		if g == nil {
			g = &explain.NodeGroup{InstanceType: n.InstanceType, Spot: n.Spot, UnitUSDPerHour: hourly}
			groups[k] = g
		}
		g.Nodes++
	}
	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	// Sorted, because a map range is not an order and this slice is summed.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].instanceType != keys[j].instanceType {
			return keys[i].instanceType < keys[j].instanceType
		}
		return !keys[i].spot && keys[j].spot
	})
	for _, k := range keys {
		b.Groups = append(b.Groups, *groups[k])
	}

	demand := map[string]*explain.NamespaceDemand{}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		if p.Phase == "Succeeded" || p.Phase == "Failed" {
			continue
		}
		req := clampedPodRequests(p)
		d := demand[p.Namespace]
		if d == nil {
			d = &explain.NamespaceDemand{Namespace: p.Namespace}
			demand[p.Namespace] = d
		}
		d.MilliCPU += req.MilliCPU
		d.MemoryBytes += req.MemoryBytes
		d.Pods++
	}
	ns := make([]string, 0, len(demand))
	for n := range demand {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	for _, n := range ns {
		b.Namespaces = append(b.Namespaces, *demand[n])
	}
	return b
}

// clampedPodRequests mirrors pkg/plan's clampedRequests exactly: the pod's
// effective request with negatives clamped to zero. It is duplicated rather
// than re-derived — pkg/explain/FINDINGS.md is explicit that a third
// definition of "requested capacity" is the thing to avoid.
func clampedPodRequests(p *model.PodSpec) model.Resources {
	req := p.Requests()
	if req.MilliCPU < 0 {
		req.MilliCPU = 0
	}
	if req.MemoryBytes < 0 {
		req.MemoryBytes = 0
	}
	return req
}

// countPricedNodes counts the nodes the composition prices, so the timeline's
// node count and the composition's agree about what a "node" is.
func countPricedNodes(snap *model.ClusterSnapshot) int {
	var n int
	for i := range snap.Nodes {
		if !snap.Nodes[i].IsFargate() {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- explain

func runExplainTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(w)
	var snaps repeatedFlag
	fs.Var(&snaps, "kube-snapshot", "cluster snapshot JSON (repeatable)")
	workload := fs.String("workload", "", "Kind/namespace/name")
	container := fs.String("container", "", "container name")
	from := fs.String("from", "", "evidence window start (RFC3339)")
	to := fs.String("to", "", "evidence window end (RFC3339)")
	catalogPath := fs.String("catalog", "", "pricing catalog JSON")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(snaps) == 0 || *workload == "" || *container == "" {
		fmt.Fprint(w, explainUsage)
		return fmt.Errorf("explain: --kube-snapshot, --workload and --container are required")
	}
	ref, err := parseWorkloadRef(*workload)
	if err != nil {
		return err
	}
	catalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	series, err := loadSnapshotSeries(snaps)
	if err != nil {
		return err
	}
	cluster := series[0].ClusterID
	key := model.ContainerKey{Workload: ref, Container: *container}

	// Replay through the REAL recommender, in the same observe-then-ask order
	// pkg/api's Ingest/Plan runs, and fill the evidence substrate from the
	// same snapshots. The window is resolved to concrete timestamps BEFORE
	// anything is computed and echoed in the output, so the answer is
	// replayable.
	// The default window is the span the EVIDENCE covers, not the span the
	// snapshot timestamps cover: one snapshot can carry days of usage samples,
	// and a window taken from the snapshot instants alone would be empty for a
	// single-snapshot history while the substrate holds three days of it.
	start, end := evidenceSpan(series)
	if *from != "" {
		if start, err = time.Parse(time.RFC3339, *from); err != nil {
			return fmt.Errorf("--from: %w", err)
		}
	}
	if *to != "" {
		if end, err = time.Parse(time.RFC3339, *to); err != nil {
			return fmt.Errorf("--to: %w", err)
		}
	}
	if !end.After(start) {
		return fmt.Errorf("explain: the evidence window [%s, %s] is empty",
			start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	}

	store, err := evidence.NewMemory(evidence.Config{})
	if err != nil {
		return err
	}
	rec, err := recommend.New(recommend.DefaultConfig())
	if err != nil {
		return err
	}
	for _, s := range series {
		rec.ObserveSnapshot(s)
		if err := observeUsage(store, cluster, s); err != nil {
			return err
		}
		if err := store.ObservePoint(cluster, evidence.TimelinePoint{
			At: s.Timestamp, CostUSDPerHour: catalog.SnapshotCost(s).HourlyUSD,
			Nodes: countPricedNodes(s),
		}); err != nil {
			return err
		}
	}

	var found *recommend.Recommendation
	for _, r := range rec.Recommendations(series[len(series)-1]) {
		if r.Key == key {
			found = &r
			break
		}
	}

	req := explain.ExplainRequest{
		Cluster: cluster,
		Subject: evidence.ContainerSubject(cluster, key),
		From:    start, To: end,
		Store: store,
		Rec:   found,
	}
	payload, err := explain.BuildExplain(req)
	if err != nil {
		return fmt.Errorf("explain: %w", err)
	}
	// §5.7's publish gate, before anything is shown.
	if err := payload.Verify(explain.Resolver{Store: store}); err != nil {
		return fmt.Errorf("explain: the answer has a citation that does not resolve, so it is not "+
			"publishable: %w", err)
	}
	if *jsonOut {
		return writeJSON(w, payload)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "kilter explain — %s over [%s, %s]\n\n", key.String(),
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	b.WriteString(payload.Prose())
	b.WriteString("\n")
	_, err = io.WriteString(w, b.String())
	return err
}

// observeUsage feeds one snapshot's measured container usage into the
// substrate, which is what BuildExplain reads its digests and series from.
func observeUsage(store *evidence.Memory, cluster string, snap *model.ClusterSnapshot) error {
	for i := range snap.Usage {
		u := &snap.Usage[i]
		at := u.Timestamp
		if at.IsZero() {
			at = snap.Timestamp
		}
		// model.Usage carries CPU, memory and a window and nothing else: no
		// throttle ratio, no restart or OOM delta. Those reach the substrate
		// through evidence events from other collectors, so they are left
		// zero here rather than fabricated — "signal absent" is a state
		// pkg/decision already knows how to read, and a zero throttle ratio
		// invented by the wiring would be a claim nobody measured.
		if err := store.ObserveSample(evidence.ContainerSubject(cluster, u.Key), evidence.Sample{
			At: at, MilliCPU: u.MilliCPU, MemoryBytes: u.MemoryBytes,
		}); err != nil {
			return fmt.Errorf("explain: record usage: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- ledger

// ledgerFile is the shape `kilter ledger --json` writes.
type ledgerFile struct {
	Entries []api.LedgerEntry `json:"entries"`
}

// loadLedgerActions projects Kilter's own audit ledger into the shape the
// attribution needs, filtered to this cluster and window.
//
// This is pkg/explain/FINDINGS.md §1's mapping, field for field, and the two
// things it insists on are both here:
//
//   - Applied must be EXACT. A dry-run moved no money, so counting one would
//     attribute a cost change to a plan that changed nothing — which is the
//     classic attribution lie with a plan attached. Only StatusDone counts;
//     StatusDryRun is deliberately excluded even though actuate.Report counts
//     it as done for its own purposes.
//   - NodesAdded stays 0 because no plan type provisions a node today. A
//     field that could only ever be wrong is left at zero rather than guessed.
//
// Finished has no ledger field, so it is left zero and pkg/explain falls back
// to At (TestZeroFinishedTimeFallsBackToStart).
func loadLedgerActions(path, cluster string, from, to time.Time) ([]explain.LedgerAction, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--ledger: %w", err)
	}
	var lf ledgerFile
	if err := json.Unmarshal(raw, &lf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make([]explain.LedgerAction, 0, len(lf.Entries))
	for _, e := range lf.Entries {
		c := e.Cluster
		if c == "" {
			c = cluster
		}
		if c != cluster || e.At.Before(from) || e.At.After(to) {
			continue
		}
		a := explain.LedgerAction{
			At: e.At, Cluster: c, Fingerprint: e.Fingerprint,
			Mode: e.Mode, Risk: e.Risk,
			Applied:             e.Mode == "apply" && e.Done > 0,
			CostBeforeHourlyUSD: e.CostBeforeHourlyUSD,
			ProjectedHourlyUSD:  e.ProjectedHourlyUSD,
		}
		for _, s := range e.Steps {
			if s.Status != actuate.StatusDone {
				continue
			}
			switch s.Step.Type {
			case plan.StepDeleteNode:
				a.NodesRemoved++
			case plan.StepResizeWorkload:
				a.Resizes++
			}
		}
		out = append(out, a)
	}
	// Sorted: a ledger read in file order would make the attribution depend on
	// the order entries happened to be appended.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out, nil
}

// ---------------------------------------------------------------- shared

// loadSnapshotSeries reads every snapshot and returns them in timestamp order.
//
// Order is imposed here rather than trusted from the command line: a replay
// whose result depends on the order paths were typed in is not a function of
// its inputs. Two snapshots sharing a timestamp are rejected for the same
// reason pkg/backtest rejects them — a tie has no defined replay order.
func loadSnapshotSeries(paths []string) ([]*model.ClusterSnapshot, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("no snapshot supplied")
	}
	out := make([]*model.ClusterSnapshot, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("--kube-snapshot: %w", err)
		}
		var snap model.ClusterSnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, &snap)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	for i := 1; i < len(out); i++ {
		if out[i].Timestamp.Equal(out[i-1].Timestamp) {
			return nil, fmt.Errorf("two snapshots share the timestamp %s; a tie has no defined replay order",
				out[i].Timestamp.UTC().Format(time.RFC3339))
		}
		if out[i].ClusterID != out[0].ClusterID {
			return nil, fmt.Errorf("snapshots name different clusters (%q and %q)",
				out[0].ClusterID, out[i].ClusterID)
		}
	}
	return out, nil
}

// evidenceSpan is the closed interval every observation in the series falls
// inside — snapshot instants and usage sample timestamps alike.
func evidenceSpan(series []*model.ClusterSnapshot) (time.Time, time.Time) {
	start, end := series[0].Timestamp, series[len(series)-1].Timestamp
	for _, s := range series {
		if s.Timestamp.Before(start) {
			start = s.Timestamp
		}
		if s.Timestamp.After(end) {
			end = s.Timestamp
		}
		for i := range s.Usage {
			at := s.Usage[i].Timestamp
			if at.IsZero() {
				continue
			}
			if at.Before(start) {
				start = at
			}
			if at.After(end) {
				end = at
			}
		}
	}
	// The window is half-open in pkg/evidence, so the last observation must
	// fall strictly inside it.
	return start, end.Add(time.Second)
}

// parseWorkloadRef parses Kind/namespace/name.
func parseWorkloadRef(s string) (model.WorkloadRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return model.WorkloadRef{}, fmt.Errorf("--workload %q: want Kind/namespace/name, e.g. Deployment/default/api", s)
	}
	return model.WorkloadRef{Kind: model.WorkloadKind(parts[0]), Namespace: parts[1], Name: parts[2]}, nil
}
