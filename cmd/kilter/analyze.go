package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/agenticode/kilter/pkg/collect"
	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
	"github.com/agenticode/kilter/pkg/recommend"
)

func runAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG, in-cluster, ~/.kube/config)")
	namespace := fs.String("namespace", "", "restrict analysis to one namespace")
	watch := fs.Duration("watch", 0, "keep sampling for this long to unlock confident rightsizing (e.g. 30m, 1h)")
	interval := fs.Duration("interval", 30*time.Second, "sampling interval in watch mode")
	catalogPath := fs.String("catalog", "", "custom pricing catalog JSON (default: embedded baseline)")
	dumpPath := fs.String("dump-snapshot", "", "also write the final snapshot to this file (for kilter simulate)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of the report")
	topN := fs.Int("top", 10, "how many overprovisioned workloads to show")
	fs.Parse(args)

	catalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	client, metrics, err := kubeClients(*kubeconfig)
	if err != nil {
		return err
	}
	col := &collect.Collector{Client: client, Metrics: metrics, ClusterID: "analyze", Namespace: *namespace}

	ctx, cancel := signalContext()
	defer cancel()

	// Rightsizing needs history. In watch mode we accumulate samples and relax
	// the thresholds to the observed window; instant mode skips rightsizing.
	recCfg := recommend.DefaultConfig()
	var recommender *recommend.Recommender
	var snap *model.ClusterSnapshot

	if *watch > 0 {
		recCfg.MinWindow = *watch / 2
		recCfg.MinSamples = 5
		recommender, err = recommend.New(recCfg)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(*watch)
		n := 0
		for time.Now().Before(deadline) && ctx.Err() == nil {
			snap, err = col.Snapshot(ctx)
			if err != nil {
				return err
			}
			recommender.ObserveSnapshot(snap)
			n++
			fmt.Fprintf(os.Stderr, "\rsampling… %d snapshots, %s remaining ",
				n, time.Until(deadline).Round(time.Second))
			select {
			case <-ctx.Done():
			case <-time.After(*interval):
			}
		}
		fmt.Fprintln(os.Stderr)
	} else {
		snap, err = col.Snapshot(ctx)
		if err != nil {
			return err
		}
	}
	if snap == nil {
		return fmt.Errorf("no snapshot collected")
	}
	if *dumpPath != "" {
		raw, _ := json.MarshalIndent(snap, "", "  ")
		if err := os.WriteFile(*dumpPath, raw, 0o644); err != nil {
			return fmt.Errorf("dump snapshot: %w", err)
		}
	}

	var recs []recommend.Recommendation
	if recommender != nil {
		recs = recommender.Recommendations(snap)
	}
	planCfg := plan.DefaultConfig()
	planCfg.ApplyRecommendations = len(recs) > 0
	p, err := plan.Build(snap, recs, catalog, planCfg)
	if err != nil {
		return err
	}

	if *jsonOut {
		out := map[string]any{
			"cost":            catalog.SnapshotCost(snap),
			"plan":            p,
			"recommendations": recs,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printReport(snap, recs, p, catalog, *topN, *watch)
	return nil
}

func loadCatalog(path string) (*pricing.Catalog, error) {
	if path == "" {
		return pricing.Embedded(), nil
	}
	return pricing.LoadFile(path)
}

// wasteRow aggregates one workload's requested vs used resources.
type wasteRow struct {
	workload  model.WorkloadRef
	container string
	requested model.Resources
	used      model.Resources
	replicas  int
	waste     float64
}

func computeWaste(snap *model.ClusterSnapshot) []wasteRow {
	type agg struct {
		req, used model.Resources
		replicas  int
		samples   int
	}
	byKey := map[model.ContainerKey]*agg{}
	for i := range snap.Pods {
		pod := &snap.Pods[i]
		if pod.Phase != "Running" {
			continue
		}
		for _, c := range pod.Containers {
			key := model.ContainerKey{Workload: pod.Workload, Container: c.Name}
			a := byKey[key]
			if a == nil {
				a = &agg{}
				byKey[key] = a
			}
			a.req = a.req.Add(c.Requests)
			a.replicas++
		}
	}
	for _, u := range snap.Usage {
		if a := byKey[u.Key]; a != nil {
			a.used = a.used.Add(model.Resources{MilliCPU: u.MilliCPU, MemoryBytes: u.MemoryBytes})
			a.samples++
		}
	}
	var rows []wasteRow
	for key, a := range byKey {
		if a.req.IsZero() || a.samples == 0 {
			continue
		}
		// Usage may hold several samples per container; average them.
		perSample := float64(a.samples)
		used := model.Resources{
			MilliCPU:    int64(float64(a.used.MilliCPU) / perSample * float64(a.replicas)),
			MemoryBytes: int64(float64(a.used.MemoryBytes) / perSample * float64(a.replicas)),
		}
		cpuWaste, memWaste := 0.0, 0.0
		if a.req.MilliCPU > 0 {
			cpuWaste = 1 - float64(used.MilliCPU)/float64(a.req.MilliCPU)
		}
		if a.req.MemoryBytes > 0 {
			memWaste = 1 - float64(used.MemoryBytes)/float64(a.req.MemoryBytes)
		}
		waste := (cpuWaste + memWaste) / 2
		if waste < 0 {
			waste = 0
		}
		rows = append(rows, wasteRow{
			workload: key.Workload, container: key.Container,
			requested: a.req, used: used, replicas: a.replicas, waste: waste,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].waste != rows[j].waste {
			return rows[i].waste > rows[j].waste
		}
		return rows[i].workload.String() < rows[j].workload.String()
	})
	return rows
}

func printReport(snap *model.ClusterSnapshot, recs []recommend.Recommendation, p *plan.Plan,
	catalog *pricing.Catalog, topN int, watched time.Duration) {

	s := newStyler()
	cost := catalog.SnapshotCost(snap)
	workers, cp := 0, 0
	for i := range snap.Nodes {
		if _, ok := snap.Nodes[i].Labels["node-role.kubernetes.io/control-plane"]; ok {
			cp++
		} else {
			workers++
		}
	}
	running := 0
	for i := range snap.Pods {
		if snap.Pods[i].Phase == "Running" {
			running++
		}
	}

	fmt.Println()
	fmt.Printf("  %s %s\n", s.bold(s.cyan("◆ Kilter")), s.bold("cluster analysis"))
	if snap.ServerVersion != "" {
		fmt.Printf("  %s\n", s.dim("kubernetes "+snap.ServerVersion))
	}
	fmt.Println()
	fmt.Printf("  %-12s %d (%d workers, %d control-plane)\n", "NODES", len(snap.Nodes), workers, cp)
	fmt.Printf("  %-12s %d running\n", "PODS", running)
	fmt.Printf("  %-12s %s/h   →   %s/month\n", "COST",
		s.bold(fmt.Sprintf("$%.3f", cost.HourlyUSD)), s.bold(usd(cost.MonthlyUSD)))
	fmt.Println()

	rows := computeWaste(snap)
	if len(rows) > 0 {
		fmt.Printf("  %s\n\n", s.bold("TOP OVERPROVISIONED WORKLOADS"))
		tb := &table{header: []string{"WORKLOAD", "CONTAINER", "REPLICAS", "REQUESTED", "USED (avg)", "WASTE"}}
		n := 0
		for _, r := range rows {
			if n >= topN {
				break
			}
			w := pct(r.waste)
			switch {
			case r.waste >= 0.7:
				w = s.red(w)
			case r.waste >= 0.4:
				w = s.yellow(w)
			default:
				w = s.green(w)
			}
			tb.add(
				fmt.Sprintf("%s/%s", r.workload.Namespace, r.workload.Name),
				r.container,
				fmt.Sprintf("%d", r.replicas),
				resStr(r.requested.MilliCPU, r.requested.MemoryBytes),
				resStr(r.used.MilliCPU, r.used.MemoryBytes),
				w,
			)
			n++
		}
		fmt.Print(tb.render("  "))
		fmt.Println()
	} else {
		fmt.Printf("  %s\n\n", s.dim("(no usage metrics available — is metrics-server installed?)"))
	}

	if len(recs) > 0 {
		fmt.Printf("  %s\n\n", s.bold("RIGHTSIZING RECOMMENDATIONS"))
		tb := &table{header: []string{"WORKLOAD", "CONTAINER", "CURRENT", "RECOMMENDED", "CONFIDENCE"}}
		for _, r := range recs {
			tb.add(
				fmt.Sprintf("%s/%s", r.Key.Workload.Namespace, r.Key.Workload.Name),
				r.Key.Container,
				resStr(r.CurrentRequest.MilliCPU, r.CurrentRequest.MemoryBytes),
				s.green(resStr(r.TargetRequest.MilliCPU, r.TargetRequest.MemoryBytes)),
				fmt.Sprintf("%.0f%%", r.Confidence*100),
			)
		}
		fmt.Print(tb.render("  "))
		fmt.Println()
	}

	fmt.Printf("  %s\n\n", s.bold("CONSOLIDATION"))
	if len(p.Removals) == 0 {
		fmt.Printf("  %s\n", s.dim("no safely removable nodes right now"))
	}
	for _, r := range p.Removals {
		fmt.Printf("  %s node %s can be drained (%s utilized, %d pods move) → saves %s/month\n",
			s.green("✔"), s.bold(r.Node), pct(r.Utilization), r.EvictedPods,
			s.green(usd(r.HourlyUSD*pricing.HoursPerMonth)))
	}
	fmt.Println()
	if p.SavingsMonthlyUSD > 0 {
		reduction := 0.0
		if p.CurrentHourlyUSD > 0 {
			reduction = (p.CurrentHourlyUSD - p.ProjectedHourlyUSD) / p.CurrentHourlyUSD
		}
		fmt.Printf("  %s %s/month (%s of node spend)\n",
			s.bold("PROJECTED SAVINGS:"), s.bold(s.green(usd(p.SavingsMonthlyUSD))), pct(reduction))
	}
	if p.GreenfieldHourlyUSD > 0 {
		fmt.Printf("  %s $%.3f/h (%s/month) %s\n",
			"GREENFIELD FLOOR:", p.GreenfieldHourlyUSD,
			usd(p.GreenfieldHourlyUSD*pricing.HoursPerMonth),
			s.dim("— cheapest possible repack of current workloads"))
	}
	fmt.Println()
	if watched == 0 {
		fmt.Printf("  %s\n\n", s.dim("tip: add --watch 30m to sample usage over time and unlock rightsizing\n  recommendations with confidence scores."))
	}
	_ = strings.TrimSpace("")
}
