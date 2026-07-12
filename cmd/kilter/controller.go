package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/actuate"
	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/collect"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/safety"
)

func runController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ExitOnError)
	brainURL := fs.String("brain-url", envOr("KILTER_BRAIN_URL", "http://kilter-brain:8180"), "brain base URL")
	token := fs.String("token", os.Getenv("KILTER_TOKEN"), "bearer token for the brain API")
	clusterID := fs.String("cluster-id", envOr("KILTER_CLUSTER_ID", "default"), "cluster to act on")
	mode := fs.String("mode", envOr("KILTER_MODE", "dry-run"), "dry-run | apply")
	interval := fs.Duration("interval", 5*time.Minute, "reconcile interval")
	maxEvictions := fs.Int("max-evictions-per-hour", 20, "sliding eviction budget")
	minConfidence := fs.Float64("min-plan-savings", 1.0, "skip plans saving less than this many USD/month")
	inPlace := fs.Bool("in-place-resize", true, "attempt in-place pod resize (K8s ≥1.33)")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: in-cluster)")
	fs.Parse(args)

	if *mode != string(actuate.ModeDryRun) && *mode != string(actuate.ModeApply) {
		return flagError("--mode must be dry-run or apply")
	}
	client, metrics, err := kubeClients(*kubeconfig)
	if err != nil {
		return err
	}
	brain, err := api.NewClient(*brainURL, *token)
	if err != nil {
		return err
	}
	act, err := actuate.New(client, actuate.Config{
		Mode:                actuate.Mode(*mode),
		MaxEvictionsPerHour: *maxEvictions,
		InPlaceResize:       *inPlace,
	})
	if err != nil {
		return err
	}
	col := &collect.Collector{Client: client, Metrics: metrics, ClusterID: *clusterID}
	regress := safety.NewRegressionDetector(30*time.Minute, 24*time.Hour)
	cooldown := safety.NewCooldowns(time.Hour)

	ctx, cancel := signalContext()
	defer cancel()
	log := slog.Default().With("component", "controller", "cluster", *clusterID, "mode", *mode)
	log.Info("controller starting", "brain", *brainURL, "interval", interval.String())

	// applied remembers resize steps so regressions can be reverted.
	applied := map[model.WorkloadRef]plan.Step{}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		reconcile(ctx, log, brain, act, col, regress, cooldown, applied, *clusterID, *minConfidence)
		select {
		case <-ctx.Done():
			log.Info("controller stopping")
			return nil
		case <-ticker.C:
		}
	}
}

func reconcile(ctx context.Context, log *slog.Logger, brain *api.Client, act *actuate.Actuator,
	col *collect.Collector, regress *safety.RegressionDetector, cooldown *safety.Cooldowns,
	applied map[model.WorkloadRef]plan.Step, clusterID string, minSavings float64) {

	// 1. Regression watch: collect fresh state and revert anything that broke.
	snap, err := col.Snapshot(ctx)
	if err != nil {
		log.Error("collect failed", "err", err)
		return
	}
	for _, reg := range regress.Check(snap, time.Now()) {
		step, ok := applied[reg.Ref]
		if !ok {
			continue
		}
		log.Warn("regression detected — reverting", "workload", reg.Ref.String(), "reason", reg.Reason)
		if err := act.ResizeWorkload(ctx, step.Workload, step.Container, step.FromReq, step.FromLim); err != nil {
			log.Error("revert failed", "workload", reg.Ref.String(), "err", err)
		}
		delete(applied, reg.Ref)
	}

	// 2. Pull the current plan.
	p, err := brain.GetPlan(ctx, clusterID)
	if err != nil {
		log.Error("plan fetch failed", "err", err)
		return
	}
	if p.Empty() {
		log.Info("cluster is in kilter — nothing to do")
		return
	}
	if p.SavingsMonthlyUSD < minSavings && len(p.Rightsizing) == 0 {
		log.Info("plan below savings threshold — skipping",
			"savings", p.SavingsMonthlyUSD, "threshold", minSavings)
		return
	}

	// 3. Filter steps through cooldowns and quarantine.
	var steps []plan.Step
	for _, s := range p.Steps {
		switch s.Type {
		case plan.StepResizeWorkload:
			if regress.Quarantined(s.Workload, time.Now()) {
				log.Info("workload quarantined — skipping resize", "workload", s.Workload.String())
				continue
			}
			if !cooldown.Allow("resize/"+s.Workload.String(), time.Now()) {
				continue
			}
		case plan.StepCordonNode:
			if !cooldown.Allow("node/"+s.Node, time.Now()) {
				// Skip this node's whole removal sequence.
				continue
			}
		}
		steps = append(steps, s)
	}
	steps = dropOrphanedNodeSteps(steps)
	if len(steps) == 0 {
		return
	}
	exec := *p
	exec.Steps = steps

	// 4. Execute and record baselines for the regression watch.
	rep := act.ExecutePlan(ctx, &exec)
	log.Info("plan executed", "done", rep.Done, "failed", rep.Failed,
		"skipped", rep.Skipped, "aborted", rep.Aborted,
		"savings_month_usd", p.SavingsMonthlyUSD)
	for _, st := range rep.Steps {
		if st.Status == "done" && st.Step.Type == plan.StepResizeWorkload {
			applied[st.Step.Workload] = st.Step
			regress.RecordChange(st.Step.Workload, snap, time.Now())
		}
	}
}

// dropOrphanedNodeSteps removes evict/delete steps whose cordon step was
// filtered out (cooldown), keeping node-removal sequences atomic.
func dropOrphanedNodeSteps(steps []plan.Step) []plan.Step {
	cordoned := map[string]bool{}
	for _, s := range steps {
		if s.Type == plan.StepCordonNode {
			cordoned[s.Node] = true
		}
	}
	var out []plan.Step
	for _, s := range steps {
		if (s.Type == plan.StepEvictPod || s.Type == plan.StepDeleteNode) && !cordoned[s.Node] {
			continue
		}
		out = append(out, s)
	}
	return out
}

type flagError string

func (e flagError) Error() string { return string(e) }

var _ = strings.TrimSpace
