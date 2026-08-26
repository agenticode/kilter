package fargate

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/agenticode/kilter/pkg/decision"
	"github.com/agenticode/kilter/pkg/domain"
	"github.com/agenticode/kilter/pkg/guard"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/pricing/commit"
	"github.com/agenticode/kilter/pkg/recommend"
)

// group is one workload's Fargate pods: the representative pod whose container
// template is resized, and how many replicas are billing at that shape.
type group struct {
	ref      model.WorkloadRef
	pod      *model.PodSpec
	replicas int
}

// candidate is one lever's proposal for a workload.
type candidate struct {
	cfg        pricing.FargateConfig
	containers []ContainerChange
	change     string
	confidence float64
	floorBytes int64 // memory the proposal is not allowed to go below
}

// Recommend derives the current recommendations for every Fargate workload.
//
// At most ONE recommendation is produced per workload. Two conflicting resizes
// for the same pod would race in the executor, so when both levers fire the
// cheaper proposal wins, ties going to the tier move — the sizing policy's own
// conclusion, which carries pkg/recommend's headroom, OOM floors and behaviour
// class, rather than the shave's bare peak-plus-noise floor.
func (d *Domain) Recommend(now time.Time, ledger domain.Netter) []domain.Recommendation {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.last == nil {
		return nil // nothing learned: report nothing, claim nothing
	}
	snap := d.last

	// Post-change health first. safety.RegressionDetector reports each
	// regression exactly once, so park the verdict where a second Recommend at
	// the same instant will still find it.
	for _, reg := range d.regress.Check(snap, now) {
		a, ok := d.applied[reg.Ref]
		if !ok {
			continue // regressed after somebody else's change; not ours to revert
		}
		a.RegressionReason, a.DetectedAt = reg.Reason, reg.DetectedAt
		d.reverts[reg.Ref] = a
		delete(d.applied, reg.Ref)
	}

	groups := groupWorkloads(snap)

	recsByKey := map[model.ContainerKey]recommend.Recommendation{}
	for _, r := range d.rec.Recommendations(snap) {
		recsByKey[r.Key] = r
	}

	out := make([]domain.Recommendation, 0, len(groups))
	for _, g := range groups {
		if rec := d.recommendWorkload(g, recsByKey, now, ledger); rec != nil {
			out = append(out, *rec)
		}
	}
	domain.SortRecommendations(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// groupWorkloads collapses the snapshot's Fargate pods into one entry per
// workload, in snapshot order. Bare pods, Jobs and CronJobs are excluded for
// the same reason pkg/recommend excludes them: their restart semantics make a
// resize meaningless or destructive.
func groupWorkloads(snap *model.ClusterSnapshot) []*group {
	var groups []*group
	idx := map[model.WorkloadRef]int{}
	for i := range snap.Pods {
		p := &snap.Pods[i]
		switch p.Workload.Kind {
		case model.KindBarePod, model.KindJob, model.KindCronJob:
			continue
		}
		if p.Phase != "" && p.Phase != "Running" {
			continue
		}
		if len(p.Containers) == 0 {
			continue
		}
		if j, ok := idx[p.Workload]; ok {
			groups[j].replicas++
			continue
		}
		idx[p.Workload] = len(groups)
		groups = append(groups, &group{ref: p.Workload, pod: p, replicas: 1})
	}
	return groups
}

// recommendWorkload projects one workload into at most one recommendation.
func (d *Domain) recommendWorkload(g *group, recsByKey map[model.ContainerKey]recommend.Recommendation,
	now time.Time, ledger domain.Netter) *domain.Recommendation {

	mode := guard.ModeFor(d.last, g.ref, d.cfg.DefaultMode)
	if mode == guard.ModeOff {
		return nil // "Kilter never touches or moves this workload" — not even to report
	}

	curCfg, src, err := pricing.FargatePodConfig(g.pod)
	if err != nil {
		// Over the 16 vCPU / 120 GB ceiling: the pod cannot be scheduled on
		// Fargate at all, so there is no bill to optimize and no honest price
		// to quote. pkg/pricing surfaces it as a warning; inventing a
		// recommendation here would be worse than silence.
		return nil
	}
	eff := pricing.FargateEffectiveRequests(g.pod)

	if a, ok := d.reverts[g.ref]; ok {
		return d.revertRec(g, a, curCfg, eff, src, mode)
	}

	quarantined := d.regress.Quarantined(g.ref, now)

	var chosen *candidate
	if c := d.tierMove(g, recsByKey, now); c != nil && c.cfg != curCfg {
		chosen = c
	}
	if c := d.boundaryShave(g, curCfg, eff, now, quarantined); c != nil {
		if chosen == nil || d.cfg.Rates.Cost(c.cfg) < d.cfg.Rates.Cost(chosen.cfg) {
			chosen = c
		}
	}
	if chosen == nil {
		return nil
	}

	curCost := d.cfg.Rates.Cost(curCfg) * float64(g.replicas)
	propCost := d.cfg.Rates.Cost(chosen.cfg) * float64(g.replicas)
	grossMonthly := (curCost - propCost) * pricing.HoursPerMonth

	// A request change that does not cross a tier boundary saves exactly $0
	// (pkg/pricing, U1). Two distinct tiers can still price identically; that
	// is a restart for nothing.
	if grossMonthly == 0 {
		return nil
	}
	if grossMonthly > 0 {
		floor := d.cfg.MinMoveMonthlyUSD
		if chosen.change == ChangeBoundaryShave {
			floor = d.cfg.MinShaveMonthlyUSD
		}
		if grossMonthly < floor {
			return nil
		}
	}

	proposedEff := effectiveOf(chosen.containers, g.pod.InitRequests)
	target := d.targetRef(g)
	rec := domain.Recommendation{
		Target:            target,
		Current:           buildSpec(curCfg, eff, containersOf(g.pod), chosen.change, string(src), g.replicas),
		Proposed:          buildSpec(chosen.cfg, proposedEff, chosen.containers, chosen.change, "", g.replicas),
		CurrentHourlyUSD:  curCost,
		ProposedHourlyUSD: propCost,
		// Fargate cannot resize a pod in place — the pod is recreated. Claiming
		// ActionInPlace would understate disruption to the executor's eviction
		// budget and PDB accounting, so this constant is unconditional.
		Action:     domain.ActionRolling,
		Confidence: chosen.confidence,
		Risk:       riskOf(chosen, eff),
	}

	net := grossMonthly
	if ledger != nil && grossMonthly > 0 {
		before := d.usageLine(target, d.cfg.Rates.Cost(curCfg), g.replicas)
		after := d.usageLine(target, d.cfg.Rates.Cost(chosen.cfg), g.replicas)
		as := ledger.Net([]commit.UsageLine{before}, []commit.UsageLine{after})
		net = as.NetMonthlyUSD
		if as.Suppressed {
			rec.Suppressed, rec.SuppressCode = true, as.ReasonCode
			rec.ValidFrom = as.ValidFrom
			rec.Reason = as.Reason + "; "
			if net > 0 {
				net = 0
			}
		}
	}
	rec.SetSavings(grossMonthly, net)

	// Safety and policy blocks outrank the economic one in the reported code:
	// a quarantined workload must read as quarantined even if its numbers also
	// happen to be commitment-negative.
	switch {
	case quarantined:
		rec.Suppressed, rec.SuppressCode = true, domain.SuppressQuarantined
		rec.Reason = "workload is quarantined after a post-change regression; " + rec.Reason
	case !rec.Suppressed && mode == guard.ModeRecommend:
		rec.Suppressed, rec.SuppressCode = true, domain.SuppressModeRecommend
		rec.Reason = "kilter.dev/mode=recommend: reporting only; " + rec.Reason
	}

	rec.Reason += d.reasonFor(g, chosen, curCfg, grossMonthly)
	rec.Evidence = d.evidenceFor(g, chosen, curCfg, src, recsByKey, now)
	return &rec
}

// tierMove projects pkg/recommend's container decisions onto the tier table.
//
// The sizing policy is reused verbatim — percentile, headroom, behaviour class,
// OOM floor, HPA guard all belong to pkg/recommend — with one addition: no
// container is ever proposed less memory than has actually been observed. The
// policy already floors at its own histogram's peak; this floors at the peak
// this domain measured, so a disagreement between the two resolves upward.
func (d *Domain) tierMove(g *group, recsByKey map[model.ContainerKey]recommend.Recommendation,
	now time.Time) *candidate {

	containers := make([]ContainerChange, 0, len(g.pod.Containers))
	var floor int64
	minConf, sawRec := 1.0, false
	for _, c := range g.pod.Containers {
		key := model.ContainerKey{Workload: g.ref, Container: c.Name}
		target := c.Requests
		if r, ok := recsByKey[key]; ok {
			target = r.TargetRequest
			sawRec = true
			if r.Confidence < minConf {
				minConf = r.Confidence
			}
		}
		peak := d.stats[key].effectivePeak(now, d.cfg.Floors)
		if peak > target.MemoryBytes {
			target.MemoryBytes = peak
		}
		floor = satAdd(floor, peak)
		containers = append(containers, ContainerChange{Name: c.Name, Requests: target})
	}
	cfg, err := pricing.Quantize(sumRequests(containers), g.pod.InitRequests)
	if err != nil {
		return nil
	}
	conf := minConf
	if !sawRec {
		conf, _, _ = d.observationConfidence(g, now)
	}
	return &candidate{cfg: cfg, containers: containers, change: ChangeTierMove, confidence: conf, floorBytes: floor}
}

// boundaryShave is the §4.1.1 lever: a pod whose memory request sits just over a
// tier boundary is billed a whole tier more, so dropping the request to just
// under the boundary can cut the bill by a third with no change in behaviour.
//
// It is also the one recommendation in this package that can take a healthy pod
// down, so every gate below is a veto and none of them is advisory:
//
//   - the container must never have been observed OOM-killed;
//   - the workload must not be quarantined after a recent regression;
//   - observation must clear MinShaveSamples AND MinShaveWindow AND
//     MinShaveConfidence — sample count and span are checked directly as well
//     as through the score, so a fresh burst of samples cannot buy its way past
//     a short window;
//   - the shaved request must clear the observed peak by the noise band,
//     max(NoiseBandFraction×peak, NoiseSigmas×σ). A shave that lands inside the
//     band is a shave onto the workload's own jitter;
//   - CPU is never shaved. The target tier must hold the *unchanged* CPU
//     request, exactly as in the AWS worked example where dropping memory from
//     8 GB to 7.75 GB moves 2 vCPU/9 GB → 1 vCPU/8 GB while the pod keeps
//     asking for 1 vCPU.
//
// The proposal is minimally invasive: the request drops to the largest value
// that still lands on the cheaper tier, not to what the sizing policy would
// pick, so the workload keeps every byte the tier boundary lets it keep.
func (d *Domain) boundaryShave(g *group, curCfg pricing.FargateConfig, eff model.Resources,
	now time.Time, quarantined bool) *candidate {

	if quarantined {
		return nil
	}
	conf, minSamples, minWindow := d.observationConfidence(g, now)
	if minSamples < int64(d.cfg.MinShaveSamples) || minWindow < d.cfg.MinShaveWindow {
		return nil
	}
	if conf < d.cfg.MinShaveConfidence {
		return nil
	}

	// Per-container floors: observed peak plus the noise band.
	floors := make([]int64, len(g.pod.Containers))
	var required, curTotal int64
	for i, c := range g.pod.Containers {
		st := d.stats[model.ContainerKey{Workload: g.ref, Container: c.Name}]
		if st == nil || st.Samples == 0 || st.OOMSeen || st.PeakBytes <= 0 {
			return nil
		}
		floors[i] = d.requiredMemory(st, now)
		required = satAdd(required, floors[i])
		curTotal = satAdd(curTotal, max(c.Requests.MemoryBytes, 0))
	}
	if required <= 0 {
		return nil
	}

	// The cheapest tier that still holds the unchanged CPU request and the
	// noise-band floor. Canonical tier order is also cost order (pkg/pricing
	// proves it), so the quantizer's answer is the cheapest reachable tier.
	target, err := pricing.Quantize(
		model.Resources{MilliCPU: eff.MilliCPU, MemoryBytes: required}, g.pod.InitRequests)
	if err != nil || d.cfg.Rates.Cost(target) >= d.cfg.Rates.Cost(curCfg) {
		return nil
	}

	maxReq := target.MemoryBytes() - pricing.FargateOverheadBytes
	if maxReq <= 0 {
		return nil
	}
	propTotal := min(curTotal, maxReq)
	switch {
	case propTotal >= curTotal:
		// The current request already fits the cheaper tier: the pod is billed
		// above what its requests imply, which is a data problem (a stale or
		// wrong CapacityProvisioned annotation), not a shave opportunity.
		return nil
	case propTotal < required:
		return nil // inside the noise band — never
	case g.pod.InitRequests.MemoryBytes > propTotal:
		// An init container sets the effective request floor, so shrinking the
		// long-running containers cannot move the tier.
		return nil
	}

	containers, ok := distribute(g.pod.Containers, floors, curTotal-propTotal)
	if !ok {
		return nil
	}
	return &candidate{
		cfg: target, containers: containers, change: ChangeBoundaryShave,
		confidence: conf, floorBytes: required,
	}
}

// distribute takes `delta` bytes out of the containers' memory requests, never
// below each container's floor, largest request first (ties by name) so the
// result is independent of the order the pod listed them.
func distribute(cs []model.ContainerSpec, floors []int64, delta int64) ([]ContainerChange, bool) {
	out := make([]ContainerChange, len(cs))
	order := make([]int, len(cs))
	for i, c := range cs {
		out[i] = ContainerChange{Name: c.Name, Requests: c.Requests}
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if cs[ia].Requests.MemoryBytes != cs[ib].Requests.MemoryBytes {
			return cs[ia].Requests.MemoryBytes > cs[ib].Requests.MemoryBytes
		}
		return cs[ia].Name < cs[ib].Name
	})
	for _, i := range order {
		if delta <= 0 {
			break
		}
		room := cs[i].Requests.MemoryBytes - floors[i]
		if room <= 0 {
			continue
		}
		take := min(room, delta)
		out[i].Requests.MemoryBytes -= take
		delta -= take
	}
	return out, delta == 0
}

// requiredMemory is the smallest memory request this container may be given:
// the age-relaxed observed peak plus the noise band.
func (d *Domain) requiredMemory(st *stat, now time.Time) int64 {
	peak := st.effectivePeak(now, d.cfg.Floors)
	if peak <= 0 {
		return 0
	}
	band := math.Max(d.cfg.NoiseBandFraction*float64(peak), d.cfg.NoiseSigmas*st.stddev())
	if !(band >= 0) || math.IsInf(band, 0) {
		band = 0
	}
	return satAdd(peak, int64(math.Ceil(math.Min(band, math.MaxInt64/4))))
}

// observationConfidence scores how well this workload is observed, and returns
// the weakest container's sample count and window — the workload is only as
// well understood as its least-observed container.
func (d *Domain) observationConfidence(g *group, now time.Time) (score float64, samples int64, window time.Duration) {
	samples, window = math.MaxInt64, time.Duration(math.MaxInt64)
	var maxAge time.Duration
	for _, c := range g.pod.Containers {
		st := d.stats[model.ContainerKey{Workload: g.ref, Container: c.Name}]
		if st == nil {
			return 0, 0, 0
		}
		samples = min(samples, st.Samples)
		window = min(window, st.window())
		if age := now.Sub(st.Last); age > maxAge {
			maxAge = age
		}
	}
	if samples == math.MaxInt64 {
		return 0, 0, 0 // no containers
	}
	// Saturations are twice the gates, matching how pkg/recommend saturates its
	// own confidence at twice its minimums.
	c := decision.Compose(
		decision.TermHistoryDepth(int(min(samples, math.MaxInt32)), 2*d.cfg.MinShaveSamples),
		decision.TermWindowSpan(window, 2*d.cfg.MinShaveWindow),
		decision.TermFreshness(maxAge, d.cfg.MaxSampleAge),
	)
	return c.Score, samples, window
}

// revertRec proposes undoing a change this domain made, after the post-change
// watch saw the workload degrade. It is emitted even when the sizing math would
// now say something else: the regression is a measured fact and the recorded
// prior spec is the only state known to have been healthy.
func (d *Domain) revertRec(g *group, a appliedChange, curCfg pricing.FargateConfig,
	eff model.Resources, src pricing.CostSource, mode string) *domain.Recommendation {

	fromCfg, ok := pricing.FargateConfigFor(a.From.Resources)
	if !ok {
		fromCfg = curCfg // hand-built step: revert the requests, quote no saving
	}
	cur := buildSpec(curCfg, eff, containersOf(g.pod), ChangeRevert, string(src), g.replicas)
	prop := a.From.WithAttr(AttrChange, ChangeRevert)
	if cur.Equal(prop) {
		delete(d.reverts, g.ref) // already back where it started
		return nil
	}

	curCost := d.cfg.Rates.Cost(curCfg) * float64(g.replicas)
	propCost := d.cfg.Rates.Cost(fromCfg) * float64(g.replicas)
	rec := domain.Recommendation{
		Target:            d.targetRef(g),
		Current:           cur,
		Proposed:          prop,
		CurrentHourlyUSD:  curCost,
		ProposedHourlyUSD: propCost,
		Action:            domain.ActionRolling,
		Risk:              plan.RiskLow, // restoring a known-good size
		Confidence:        1,            // an observed regression, not an inference
		Reason: fmt.Sprintf("post-change regression: %s; restoring the requests in place before %s",
			a.RegressionReason, a.At.UTC().Format(time.RFC3339)),
		Evidence: []domain.Evidence{{
			Metric: "post-change-regression",
			Value:  a.RegressionReason,
			Source: "kilter/safety",
			At:     a.DetectedAt,
		}, {
			Metric: "tier",
			Value:  curCfg.String() + " → " + fromCfg.String(),
			Source: domain.SourceQuantizer,
		}},
	}
	// A revert costs money; saying so is the point. Gross is negative and net
	// tracks it, so nothing downstream can book a revert as a saving.
	gross := (curCost - propCost) * pricing.HoursPerMonth
	rec.SetSavings(gross, gross)
	if mode == guard.ModeRecommend {
		rec.Suppressed, rec.SuppressCode = true, domain.SuppressModeRecommend
		rec.Reason = "kilter.dev/mode=recommend: reporting only; " + rec.Reason
	}
	return &rec
}

// riskOf classifies a proposal. Memory is the dimension that kills pods, so any
// proposal that lowers the pod's memory request is at least medium risk, and a
// boundary shave — which by construction sits closer to the observed peak than
// the sizing policy would — always is.
func riskOf(c *candidate, cur model.Resources) string {
	if c.change == ChangeBoundaryShave {
		return plan.RiskMedium
	}
	if sumRequests(c.containers).MemoryBytes < cur.MemoryBytes {
		return plan.RiskMedium
	}
	return plan.RiskLow
}

func (d *Domain) targetRef(g *group) domain.TargetRef {
	return domain.TargetRef{
		Domain: Kind,
		Scope:  d.scope,
		ID:     targetID(g.ref),
		Name:   g.ref.Namespace + "/" + g.ref.Name,
	}
}

// usageLine renders a workload's Fargate spend as a commitment usage line.
//
// ComputeSPRate is left unknown (0) on purpose: Kilter does not have Fargate
// Savings-Plan rates, and pkg/pricing/commit's documented behaviour for an
// unknown rate is to assume the commitment is fully stranded and the usage free
// at the margin. That under-claims savings and can never invent them, which is
// the right way to be wrong about somebody's bill.
func (d *Domain) usageLine(ref domain.TargetRef, hourly float64, replicas int) commit.UsageLine {
	return commit.UsageLine{
		ID:       "fargate/" + ref.Scope + "/" + ref.ID,
		Kind:     commit.KindFargate,
		Region:   d.cfg.Region,
		Unit:     "pod-hours",
		Quantity: float64(replicas),
		ODRate:   hourly,
	}
}

func (d *Domain) reasonFor(g *group, c *candidate, curCfg pricing.FargateConfig, grossMonthly float64) string {
	verb := "tier move"
	if c.change == ChangeBoundaryShave {
		verb = "boundary shave"
	}
	if grossMonthly < 0 {
		return fmt.Sprintf("%s %s → %s for %d pod(s): the learned size no longer fits the current tier; costs $%.2f/mo more",
			verb, curCfg, c.cfg, g.replicas, -grossMonthly)
	}
	return fmt.Sprintf("%s %s → %s for %d pod(s), floor %s; $%.2f/mo gross",
		verb, curCfg, c.cfg, g.replicas,
		model.Resources{MemoryBytes: c.floorBytes}, grossMonthly)
}

// evidenceFor lists the observable facts behind a recommendation, in a fixed
// order. A recommendation with no evidence is a bug (domain.Recommendation.Validate
// rejects it), so the tier line is unconditional.
func (d *Domain) evidenceFor(g *group, c *candidate, curCfg pricing.FargateConfig,
	src pricing.CostSource, recsByKey map[model.ContainerKey]recommend.Recommendation,
	now time.Time) []domain.Evidence {

	_, samples, window := d.observationConfidence(g, now)
	var peak, oomAt = int64(0), time.Time{}
	var peakAt time.Time
	oom := false
	var cpuTarget int64
	sawRec := false
	for _, cs := range g.pod.Containers {
		key := model.ContainerKey{Workload: g.ref, Container: cs.Name}
		if st := d.stats[key]; st != nil {
			peak = satAdd(peak, st.effectivePeak(now, d.cfg.Floors))
			if st.PeakAt.After(peakAt) {
				peakAt = st.PeakAt
			}
			if st.OOMSeen {
				oom = true
				if st.LastOOM.After(oomAt) {
					oomAt = st.LastOOM
				}
			}
		}
		if r, ok := recsByKey[key]; ok {
			sawRec = true
			cpuTarget = satAdd(cpuTarget, r.TargetRequest.MilliCPU)
		}
	}

	ev := []domain.Evidence{{
		Metric: "tier",
		Value:  curCfg.String() + " → " + c.cfg.String(),
		Source: domain.SourceQuantizer,
	}}
	if src == pricing.SourceProvisioned {
		ev = append(ev, domain.Evidence{
			Metric: "capacity-provisioned",
			Value:  curCfg.String(),
			Source: domain.SourceAnnotation,
		})
	}
	if samples > 0 {
		ev = append(ev, domain.Evidence{
			Metric:  "mem-peak",
			Value:   model.Resources{MemoryBytes: peak}.String(),
			Window:  window.Round(time.Minute).String(),
			Samples: int(min(samples, math.MaxInt32)),
			Source:  domain.SourceMetricsAPI,
			At:      peakAt,
		})
	}
	if c.change == ChangeBoundaryShave {
		ev = append(ev, domain.Evidence{
			Metric: "mem-shave-floor",
			Value:  model.Resources{MemoryBytes: c.floorBytes}.String(),
			Source: domain.SourceQuantizer,
		})
	}
	if sawRec {
		ev = append(ev, domain.Evidence{
			Metric: "cpu-target",
			Value:  model.Resources{MilliCPU: cpuTarget}.String(),
			Source: domain.SourceRecommender,
		})
	}
	if oom {
		ev = append(ev, domain.Evidence{
			Metric: "oom-observed",
			Value:  "true",
			Source: domain.SourceMetricsAPI,
			At:     oomAt,
		})
	}
	return ev
}

// containersOf snapshots a pod's current container requests.
func containersOf(p *model.PodSpec) []ContainerChange {
	out := make([]ContainerChange, 0, len(p.Containers))
	for _, c := range p.Containers {
		out = append(out, ContainerChange{Name: c.Name, Requests: c.Requests})
	}
	return out
}

// sumRequests sums container requests with saturating arithmetic.
func sumRequests(cs []ContainerChange) model.Resources {
	var out model.Resources
	for _, c := range cs {
		out = out.Add(c.Requests)
	}
	return out
}

// effectiveOf is the pod-level request Fargate sizes from: per dimension, the
// larger of the summed long-running containers and the init-container maximum.
func effectiveOf(cs []ContainerChange, init model.Resources) model.Resources {
	return sumRequests(cs).Max(init)
}

// satAdd adds without wrapping past the int64 ceiling.
func satAdd(a, b int64) int64 {
	if a > 0 && b > math.MaxInt64-a {
		return math.MaxInt64
	}
	if a < 0 && b < math.MinInt64-a {
		return math.MinInt64
	}
	return a + b
}
