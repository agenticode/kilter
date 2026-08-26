package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/actuate"
	"github.com/agenticode/kilter/pkg/evidence"
	"github.com/agenticode/kilter/pkg/explain"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
)

// The explanation plane, served.
//
//	GET /api/v1/clusters/{id}/why-cost?from=&to=   → *explain.Attribution
//	GET /api/v1/clusters/{id}/explain?subject=     → *explain.Explanation
//
// # Verify is not optional, and neither is what it means when it fails
//
// pkg/explain's central rule (§5.6/§5.7) is that a number the reader cannot
// trace is worse than a missing one. Every Term, every Driver and the residual
// carry an evidence ID, and Verify re-resolves each of them against the SAME
// store that produced the answer. Nothing inside pkg/explain enforces that at
// serve time; these handlers do, before a byte is written.
//
// The three failure modes are answered differently on purpose, because they
// are different facts about the world:
//
//   - THE CLUSTER IS UNKNOWN → 404. Nothing was ever ingested under that id.
//   - THE SUBSTRATE CANNOT SUPPORT AN ANSWER → 422 with the reason. A window
//     holding fewer than two timeline points is not a measurement; a subject
//     with no history is not an explanation. This is the case a brain whose
//     substrate was never populated hits, and it must read as "there is not
//     enough evidence here", NOT as a server fault and emphatically not as a
//     confident answer whose citations dangle.
//   - VERIFY FAILED → 500. The answer was computed and its own citations do
//     not resolve. That is a defect in this process, and it is the one case
//     where the operator should be paged rather than reassured.

// maxExplainWindow bounds the window either route will answer for. The
// substrate is bounded, so a wider request cannot return more; refusing names
// the bound instead of returning a short answer that looks complete.
const maxExplainWindow = 400 * 24 * time.Hour

// defaultExplainWindow is the trailing span an explain request without an
// explicit window resolves to. It is resolved against the LATEST INGESTED
// SNAPSHOT, never against wall-clock time: an explanation whose window drifts
// between two identical requests is not replayable, and replayability is the
// point. The resolved instants are echoed in the payload's From/To.
const defaultExplainWindow = 24 * time.Hour

func (b *Brain) registerExplainRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/clusters/{id}/why-cost", b.auth(b.handleWhyCost))
	mux.HandleFunc("GET /api/v1/clusters/{id}/explain", b.auth(b.handleExplain))
}

// ---------------------------------------------------------------- why-cost

func (b *Brain) handleWhyCost(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("id")
	if b.snapshotFor(cluster) == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown cluster %q", cluster))
		return
	}
	// --from and --to are REQUIRED, for the reason `kilter why-cost` requires
	// them: the window is an argument, and an answer computed over a
	// wall-clock default cannot be replayed or compared to a stored one.
	from, to, err := parseWindow(r, time.Time{}, time.Time{})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	att, err := b.WhyCost(cluster, from, to)
	if err != nil {
		writeErr(w, explainStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, att)
}

// WhyCost decomposes a cluster's cost change over [from, to).
//
// It is exported because the decomposition, not the HTTP framing, is the
// product: cmd/ and pkg/mcp both want the payload, and neither should have to
// reimplement the projection or remember to call Verify.
func (b *Brain) WhyCost(cluster string, from, to time.Time) (*explain.Attribution, error) {
	if err := checkExplainWindow(from, to); err != nil {
		return nil, err
	}
	timeline, err := b.mem.Timeline(cluster, from, to)
	if err != nil {
		return nil, err
	}
	if len(timeline) < 2 {
		return nil, notEnoughEvidence{fmt.Errorf(
			"the evidence substrate holds %d timeline point(s) for %q inside [%s, %s); "+
				"ΔCost needs two observations to be a measurement. Note the window is half-open: "+
				"a point at exactly `to` is outside it",
			len(timeline), cluster, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))}
	}

	// The window edges' fleet composition comes from the TIME-KEYED SNAPSHOT
	// HISTORY, which is the other half of what this unit built. Without a
	// store there is no history, and both edges stay nil — WhyCost degrades
	// honestly in that case (only the node-count term is computable and it
	// says so in Notes) rather than inventing a composition nobody observed.
	var start, end *explain.CostBasis
	if b.st != nil {
		snaps, err := b.st.Snapshots(cluster, from, to)
		if err != nil {
			return nil, err
		}
		if len(snaps) > 0 {
			start = basisFrom(snaps[0], b.catalog)
			end = basisFrom(snaps[len(snaps)-1], b.catalog)
		}
	}

	actions := b.ledgerActions(cluster, from, to)
	att, err := explain.WhyCost(explain.Input{
		Cluster: cluster, From: from, To: to,
		Timeline: timeline,
		Start:    start, End: end,
		Actions: actions,
	})
	if err != nil {
		// A decomposition that cannot be computed is an error, not an empty
		// table.
		return nil, notEnoughEvidence{err}
	}
	// §5.7's publish gate. Nothing else enforces it.
	if err := att.Verify(explain.Resolver{Store: b.mem, Actions: actions}); err != nil {
		return nil, unverifiable{err}
	}
	return att, nil
}

// ---------------------------------------------------------------- explain

func (b *Brain) handleExplain(w http.ResponseWriter, r *http.Request) {
	cluster := r.PathValue("id")
	snap := b.snapshotFor(cluster)
	if snap == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("unknown cluster %q", cluster))
		return
	}
	subject := r.URL.Query().Get("subject")
	if strings.TrimSpace(subject) == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"subject is required: Kind/namespace/name/container for a container, "+
				"or Kind/namespace/name for the workload"))
		return
	}
	// The default window is derived from the latest ingested snapshot, not
	// from a clock, and both ends are concrete before anything is computed.
	defTo := snap.Timestamp.Add(time.Second)
	from, to, err := parseWindow(r, defTo.Add(-defaultExplainWindow), defTo)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	payload, err := b.Explain(cluster, subject, from, to)
	if err != nil {
		writeErr(w, explainStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// Explain builds the grounded explanation for one subject over [from, to).
// subject is "Kind/namespace/name/container" or "Kind/namespace/name".
func (b *Brain) Explain(cluster, subject string, from, to time.Time) (*explain.Explanation, error) {
	if err := checkExplainWindow(from, to); err != nil {
		return nil, err
	}
	ref, key, err := parseSubject(cluster, subject)
	if err != nil {
		return nil, err
	}

	// The recommendation, when there is one, comes from the production path —
	// the same recommender, against the same snapshot the API would answer
	// /recommendations from — so the explanation explains what the engine
	// would actually do rather than a re-derivation of it.
	var found *recommend.Recommendation
	if key != nil {
		recs, err := b.Recommendations(cluster)
		if err != nil {
			return nil, err
		}
		for i := range recs {
			if recs[i].Key == *key {
				found = &recs[i]
				break
			}
		}
	}

	payload, err := explain.BuildExplain(explain.ExplainRequest{
		Cluster: cluster,
		Subject: ref,
		From:    from, To: to,
		Store: b.mem,
		Rec:   found,
		// Verdict stays nil: pkg/recommend does not import pkg/decision, so
		// production has no verdict to read out (cmd/WIRING-FINDINGS.md §6.4).
		// Evaluating decision.Evaluate here would answer a question production
		// never asked, which is a different answer wearing this one's clothes.
	})
	if err != nil {
		return nil, notEnoughEvidence{err}
	}
	// §5.7's publish gate, before anything is shown.
	if err := payload.Verify(explain.Resolver{Store: b.mem}); err != nil {
		return nil, unverifiable{err}
	}
	return payload, nil
}

// parseSubject maps the query parameter to an evidence subject. A four-segment
// value is a container template and also yields the ContainerKey the
// recommender is indexed by; a three-segment value is the workload itself.
func parseSubject(cluster, s string) (evidence.SubjectRef, *model.ContainerKey, error) {
	parts := strings.Split(s, "/")
	for _, p := range parts {
		if p == "" {
			return evidence.SubjectRef{}, nil, fmt.Errorf(
				"subject %q: want Kind/namespace/name/container or Kind/namespace/name", s)
		}
	}
	ref := model.WorkloadRef{Kind: model.WorkloadKind(parts[0])}
	switch len(parts) {
	case 4:
		ref.Namespace, ref.Name = parts[1], parts[2]
		key := model.ContainerKey{Workload: ref, Container: parts[3]}
		return evidence.ContainerSubject(cluster, key), &key, nil
	case 3:
		ref.Namespace, ref.Name = parts[1], parts[2]
		return evidence.WorkloadSubject(cluster, ref), nil, nil
	default:
		return evidence.SubjectRef{}, nil, fmt.Errorf(
			"subject %q: want Kind/namespace/name/container or Kind/namespace/name", s)
	}
}

// ---------------------------------------------------------------- window

// parseWindow resolves from/to. A zero default makes the parameter required.
func parseWindow(r *http.Request, defFrom, defTo time.Time) (time.Time, time.Time, error) {
	q := r.URL.Query()
	from, err := parseInstant(q.Get("from"), defFrom, "from")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parseInstant(q.Get("to"), defTo, "to")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

func parseInstant(raw string, def time.Time, name string) (time.Time, error) {
	if raw == "" {
		if def.IsZero() {
			return time.Time{}, fmt.Errorf(
				"%s is required (RFC3339): the window is an argument, and an answer computed "+
					"over a wall-clock default cannot be replayed", name)
		}
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s: %w", name, err)
	}
	return t, nil
}

// checkExplainWindow rejects an empty, inverted or unbounded window. The
// substrate is bounded, so a ten-year request cannot return ten years; it
// would return a bounded slice that reads as if it were the whole span.
func checkExplainWindow(from, to time.Time) error {
	if from.IsZero() || to.IsZero() {
		return badRequest{fmt.Errorf("the window needs two concrete instants")}
	}
	if !to.After(from) {
		return badRequest{fmt.Errorf("window [%s, %s) is empty or inverted",
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))}
	}
	if span := to.Sub(from); span > maxExplainWindow {
		return badRequest{fmt.Errorf("window spans %v, over the %v cap; the substrate is bounded "+
			"and a wider window would return a bounded answer that reads as a complete one",
			span, maxExplainWindow)}
	}
	return nil
}

// ---------------------------------------------------------------- errors

// The three failure classes, as types rather than string matching, so the
// HTTP status is a property of the failure and not of its wording.
type (
	badRequest        struct{ error }
	notEnoughEvidence struct{ error }
	unverifiable      struct{ error }
)

func (e unverifiable) Error() string {
	return "the answer has a citation that does not resolve, so it is not publishable: " + e.error.Error()
}

func explainStatus(err error) int {
	switch err.(type) {
	case badRequest:
		return http.StatusBadRequest
	case notEnoughEvidence:
		return http.StatusUnprocessableEntity
	case unverifiable:
		return http.StatusInternalServerError
	default:
		return http.StatusUnprocessableEntity
	}
}

// ---------------------------------------------------------------- projections

// ledgerActions projects the audit ledger into the shape the attribution
// needs, filtered to this cluster and window.
//
// This is pkg/explain/FINDINGS.md §1's mapping, field for field, and it is
// the same projection cmd/kilter/explain.go's loadLedgerActions performs over
// the JSON form of the same records — lifted here rather than re-derived,
// because a second definition of "which ledger entries moved money" is a
// second answer waiting to disagree with the first.
//
// The two things §1 insists on:
//
//   - Applied must be EXACT. A dry-run moved no money, so counting one would
//     attribute a cost change to a plan that changed nothing. Only
//     actuate.StatusDone counts; StatusDryRun is deliberately excluded.
//   - NodesAdded stays 0 because no plan type provisions a node today. A field
//     that could only ever be wrong is left at zero rather than guessed.
//
// Finished has no ledger field, so it stays zero and pkg/explain falls back
// to At (TestZeroFinishedTimeFallsBackToStart).
func (b *Brain) ledgerActions(cluster string, from, to time.Time) []explain.LedgerAction {
	entries := b.Ledger(cluster).Entries
	out := make([]explain.LedgerAction, 0, len(entries))
	for _, e := range entries {
		c := e.Cluster
		if c == "" {
			c = cluster
		}
		if c != cluster || e.At.Before(from) || !e.At.Before(to) {
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
	// Sorted: LedgerReport.Entries is newest-first, and an attribution that
	// depended on the order entries happened to be appended would not be a
	// function of its inputs.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out
}

// basisFrom prices one snapshot's fleet composition — pkg/explain/FINDINGS.md
// §2's projection, with the three things it says the wiring must get right:
//
//  1. FARGATE NODES ARE EXCLUDED. A Fargate "node" is a single-pod VM billed
//     per quantized pod, not a shareable machine; pricing it per node shape
//     would inflate the fleet total and put the error inside a term instead of
//     in the residual. pkg/pricing.SnapshotCost does include Fargate pods, so
//     the difference lands in the residual with a note — honest and coarse.
//  2. AN EMPTY FLEET IS &CostBasis{At: t}, NOT nil. nil means "I could not
//     determine the composition"; a cluster created inside the window really
//     was empty at the start edge, and conflating the two silently downgrades
//     a complete answer into a residual.
//  3. NAMESPACE DEMAND IS REQUESTED CAPACITY, not usage — requests are what
//     force node count.
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
// effective request with negatives clamped to zero. Duplicated rather than
// re-derived — pkg/explain/FINDINGS.md is explicit that a THIRD definition of
// "requested capacity" is the thing to avoid, and pkg/plan's is unexported.
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
