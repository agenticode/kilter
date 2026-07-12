package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/model"
)

func runInsights(args []string) error {
	fs := flag.NewFlagSet("insights", flag.ExitOnError)
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
	ins, err := client.GetInsights(ctx, *clusterID)
	if err != nil {
		return err
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"insights": ins})
	}
	printInsights(ins, *clusterID)
	return nil
}

func printInsights(ins []model.Insight, cluster string) {
	s := newStyler()
	fmt.Println()
	fmt.Printf("  %s %s %s\n\n", s.bold(s.cyan("◆ Kilter")), s.bold("insights for"), s.bold(cluster))
	if len(ins) == 0 {
		fmt.Printf("  %s\n\n", s.green("✔ no findings — cluster behavior is within learned norms"))
		return
	}
	for _, i := range ins {
		icon := s.dim("•")
		switch i.Severity {
		case "critical":
			icon = s.red("✖")
		case "warning":
			icon = s.yellow("▲")
		case "info":
			icon = s.cyan("ℹ")
		}
		target := i.Node
		if i.Workload.Name != "" {
			target = i.Workload.Namespace + "/" + i.Workload.Name
			if i.Container != "" {
				target += "/" + i.Container
			}
		}
		if target != "" {
			target = s.bold(target) + "  "
		}
		fmt.Printf("  %s %s[%s] %s\n", icon, target, i.Kind, i.Message)
	}
	fmt.Println()
}
