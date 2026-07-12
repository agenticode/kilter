package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/agenticode/kilter/pkg/actuate"
	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/plan"
)

func trustClient(fs *flag.FlagSet, args []string) (*api.Client, string, bool, error) {
	brainURL := fs.String("brain-url", envOr("KILTER_BRAIN_URL", "http://localhost:8180"), "brain base URL")
	token := fs.String("token", os.Getenv("KILTER_TOKEN"), "bearer token")
	clusterID := fs.String("cluster-id", envOr("KILTER_CLUSTER_ID", "default"), "cluster")
	jsonOut := fs.Bool("json", false, "raw JSON output")
	fs.Parse(args)
	client, err := api.NewClient(*brainURL, *token)
	return client, *clusterID, *jsonOut, err
}

// runLedger prints the audit ledger: every executed plan plus the measured
// cost curve, so claimed savings can be checked against reality.
func runLedger(args []string) error {
	fs := flag.NewFlagSet("ledger", flag.ExitOnError)
	client, cluster, jsonOut, err := trustClient(fs, args)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	rep, err := client.GetLedger(ctx, cluster)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	s := newStyler()
	fmt.Println()
	fmt.Printf("  %s %s %s\n\n", s.bold(s.cyan("◆ Kilter")), s.bold("audit ledger for"), s.bold(cluster))
	if len(rep.Entries) == 0 {
		fmt.Printf("  %s\n\n", s.dim("no executed plans recorded yet"))
		return nil
	}
	tb := &table{header: []string{"WHEN", "MODE", "FINGERPRINT", "STEPS", "COST BEFORE", "PROJECTED", "CLAIMED/MO"}}
	for _, e := range rep.Entries {
		status := fmt.Sprintf("%d ok", e.Done)
		if e.Failed > 0 {
			status += s.red(fmt.Sprintf(" %d failed", e.Failed))
		}
		tb.add(e.At.Format("01-02 15:04"), e.Mode, e.Fingerprint, status,
			fmt.Sprintf("$%.3f/h", e.CostBeforeHourlyUSD),
			fmt.Sprintf("$%.3f/h", e.ProjectedHourlyUSD),
			s.green(usd(e.ProjectedMonthlySavings)))
	}
	fmt.Print(tb.render("  "))
	fmt.Println()
	if len(rep.CostTimeline) > 0 {
		latest := rep.CostTimeline[len(rep.CostTimeline)-1]
		fmt.Printf("  measured cost now: %s/h", s.bold(fmt.Sprintf("$%.3f", latest.HourlyUSD)))
		if rep.RealizedMonthlyUSD != 0 {
			fmt.Printf("   realized: %s/month", s.bold(s.green(usd(rep.RealizedMonthlyUSD))))
		}
		fmt.Printf("\n  %s\n", s.dim(rep.Method))
	}
	fmt.Println()
	return nil
}

// runApprove approves a plan fingerprint (or lists approvals with no args).
func runApprove(args []string) error {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	var fingerprint string
	// Allow `kilter approve <fp> --cluster x` and `kilter approve --cluster x <fp>`.
	var rest []string
	for _, a := range args {
		if len(a) > 0 && a[0] != '-' && fingerprint == "" && !isFlagValue(rest) {
			fingerprint = a
			continue
		}
		rest = append(rest, a)
	}
	client, cluster, jsonOut, err := trustClient(fs, rest)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	if fingerprint == "" {
		aps, err := client.GetApprovals(ctx, cluster)
		if err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(os.Stdout).Encode(aps)
		}
		if len(aps) == 0 {
			fmt.Println("no valid approvals — approve the current plan with: kilter approve <fingerprint>")
			return nil
		}
		for _, a := range aps {
			fmt.Printf("%s approved %s (expires %s)\n", a.Fingerprint,
				a.ApprovedAt.Format("15:04:05"), a.ExpiresAt.Format("01-02 15:04"))
		}
		return nil
	}
	if err := client.Approve(ctx, cluster, fingerprint); err != nil {
		return err
	}
	fmt.Printf("approved %s for cluster %s (valid 24h) — the controller executes it on its next reconcile\n",
		fingerprint, cluster)
	return nil
}

// isFlagValue reports whether the previous token expects a value (so bare
// words after flags aren't mistaken for the fingerprint).
func isFlagValue(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	last := rest[len(rest)-1]
	switch last {
	case "-brain-url", "--brain-url", "-token", "--token", "-cluster-id", "--cluster-id":
		return true
	}
	return false
}

// runUndo reverts the most recent applied ledger entry: resizes go back to
// their From values and cordoned nodes are uncordoned. Deleted nodes cannot
// be resurrected — the cloud provider owns that — and are reported as such.
func runUndo(args []string) error {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: in-cluster, ~/.kube/config)")
	dry := fs.Bool("dry-run", false, "print what would be reverted without doing it")
	client, cluster, _, err := trustClient(fs, args)
	if err != nil {
		return err
	}
	_ = kubeconfig // parsed by trustClient's fs.Parse via shared FlagSet
	ctx, cancel := signalContext()
	defer cancel()
	rep, err := client.GetLedger(ctx, cluster)
	if err != nil {
		return err
	}
	var target *api.LedgerEntry
	for i := range rep.Entries { // entries are newest-first
		if rep.Entries[i].Mode == "apply" && rep.Entries[i].Done > 0 {
			target = &rep.Entries[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no applied ledger entry to undo")
	}

	var inverse []plan.Step
	skipped := 0
	seq := 1
	for _, st := range target.Steps {
		if st.Status != "done" {
			continue
		}
		switch st.Step.Type {
		case plan.StepResizeWorkload:
			inverse = append(inverse, plan.Step{
				Seq: seq, Type: plan.StepResizeWorkload, Risk: plan.RiskLow,
				Workload: st.Step.Workload, Container: st.Step.Container,
				ToReq: st.Step.FromReq, ToLim: st.Step.FromLim,
				Detail: fmt.Sprintf("undo: %s back to %s", st.Step.Workload.Name, st.Step.FromReq),
			})
			seq++
		case plan.StepCordonNode:
			inverse = append(inverse, plan.Step{
				Seq: seq, Type: "uncordon-node", Node: st.Step.Node, Risk: plan.RiskLow,
				Detail: "undo: uncordon " + st.Step.Node,
			})
			seq++
		case plan.StepDeleteNode, plan.StepEvictPod:
			skipped++ // irreversible: the scheduler/cloud already moved on
		}
	}
	fmt.Printf("undoing plan %s executed %s: %d step(s), %d irreversible (deletes/evictions)\n",
		target.Fingerprint, target.At.Format("01-02 15:04"), len(inverse), skipped)
	if len(inverse) == 0 {
		return fmt.Errorf("nothing reversible in the latest applied entry")
	}
	if *dry {
		for _, s := range inverse {
			fmt.Printf("  would: %s\n", s.Detail)
		}
		return nil
	}

	kube, _, err := kubeClients(*kubeconfig)
	if err != nil {
		return err
	}
	act, err := actuate.New(kube, actuate.Config{Mode: actuate.ModeApply})
	if err != nil {
		return err
	}
	for _, s := range inverse {
		var err error
		if s.Type == "uncordon-node" {
			err = act.Uncordon(ctx, s.Node)
		} else {
			err = act.ResizeWorkload(ctx, s.Workload, s.Container, s.ToReq, s.ToLim)
		}
		if err != nil {
			return fmt.Errorf("undo step %d: %w", s.Seq, err)
		}
		fmt.Printf("  ✔ %s\n", s.Detail)
	}
	fmt.Println("undo complete")
	return nil
}
