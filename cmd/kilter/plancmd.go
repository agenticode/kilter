package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/plan"
	"github.com/agenticode/kilter/pkg/pricing"
)

func runPlanCmd(args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	brainURL := fs.String("brain-url", envOr("KILTER_BRAIN_URL", "http://localhost:8180"), "brain base URL")
	token := fs.String("token", os.Getenv("KILTER_TOKEN"), "bearer token")
	clusterID := fs.String("cluster-id", envOr("KILTER_CLUSTER_ID", "default"), "cluster")
	jsonOut := fs.Bool("json", false, "raw JSON output")
	fs.Parse(args)

	client, err := api.NewClient(*brainURL, *token)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	p, err := client.GetPlan(ctx, *clusterID)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(p)
	}
	printPlan(p)
	return nil
}

func printPlan(p *plan.Plan) {
	s := newStyler()
	fmt.Println()
	fmt.Printf("  %s %s %s\n", s.bold(s.cyan("◆ Kilter")), s.bold("plan for"), s.bold(p.ClusterID))
	fmt.Printf("  %s\n\n", s.dim(fmt.Sprintf("created %s · risk: %s", p.CreatedAt.Format("2006-01-02 15:04:05 MST"), p.Risk)))
	if p.Empty() {
		fmt.Printf("  %s\n\n", s.green("✔ cluster is in kilter — no changes proposed"))
		return
	}
	tb := &table{header: []string{"#", "ACTION", "TARGET", "DETAIL", "RISK"}}
	for _, st := range p.Steps {
		target := st.Node
		if st.Type == plan.StepResizeWorkload {
			target = st.Workload.Namespace + "/" + st.Workload.Name
		}
		if st.Type == plan.StepEvictPod {
			target = st.Pod
		}
		risk := st.Risk
		switch risk {
		case plan.RiskHigh:
			risk = s.red(risk)
		case plan.RiskMedium:
			risk = s.yellow(risk)
		default:
			risk = s.green(risk)
		}
		detail := st.Detail
		if len(detail) > 60 {
			detail = detail[:57] + "…"
		}
		tb.add(fmt.Sprintf("%d", st.Seq), string(st.Type), target, detail, risk)
	}
	fmt.Print(tb.render("  "))
	fmt.Println()
	fmt.Printf("  current  %s/h   →   projected  %s/h\n",
		fmt.Sprintf("$%.3f", p.CurrentHourlyUSD), fmt.Sprintf("$%.3f", p.ProjectedHourlyUSD))
	if p.SavingsMonthlyUSD > 0 {
		fmt.Printf("  %s %s/month\n", s.bold("savings:"), s.bold(s.green(usd(p.SavingsMonthlyUSD))))
	}
	if p.GreenfieldHourlyUSD > 0 {
		fmt.Printf("  %s $%.3f/h\n", s.dim("greenfield floor:"), p.GreenfieldHourlyUSD)
	}
	fmt.Println()
	_ = pricing.HoursPerMonth
}
