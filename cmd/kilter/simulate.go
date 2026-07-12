package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/agenticode/kilter/pkg/model"
	"github.com/agenticode/kilter/pkg/plan"
)

// runSimulate replays a recorded snapshot through the decision engine —
// the same code path the brain uses — with zero cluster access. Use it to
// review what Kilter *would* do, diff decisions across config changes, or
// reproduce a decision from a saved snapshot in a bug report.
func runSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	snapPath := fs.String("snapshot", "", "snapshot JSON file (from kilter analyze --dump-snapshot)")
	catalogPath := fs.String("catalog", "", "custom pricing catalog JSON")
	jsonOut := fs.Bool("json", false, "raw JSON output")
	maxRemovals := fs.Int("max-node-removals", 3, "consolidation bound per plan")
	minUtil := fs.Float64("min-node-utilization", 0.5, "nodes below this dominant utilization are candidates")
	headroom := fs.Float64("min-headroom", 0.10, "post-consolidation free capacity floor")
	fs.Parse(args)

	if *snapPath == "" {
		return fmt.Errorf("--snapshot is required")
	}
	raw, err := os.ReadFile(*snapPath)
	if err != nil {
		return err
	}
	var snap model.ClusterSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("parse snapshot: %w", err)
	}
	catalog, err := loadCatalog(*catalogPath)
	if err != nil {
		return err
	}
	cfg := plan.DefaultConfig()
	cfg.MaxNodeRemovals = *maxRemovals
	cfg.MinNodeUtilization = *minUtil
	cfg.MinClusterHeadroom = *headroom
	cfg.ApplyRecommendations = false // simulate consolidates declared requests

	p, err := plan.Build(&snap, nil, catalog, cfg)
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
