// Command kilter is the single binary behind every Kilter role:
//
//	kilter analyze     instant savings report from any kubeconfig (read-only)
//	kilter agent       in-cluster collector shipping snapshots to the brain
//	kilter brain       central decision service (API + learning + plans)
//	kilter controller  executes brain plans under the safety envelope
//	kilter plan        fetch & print the current plan from a brain
//	kilter simulate    replay a recorded snapshot through the decision engine
//	kilter version     build information
package main

import (
	"fmt"
	"log/slog"
	"os"
)

// Set via -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

const rootUsage = `kilter — keep your Kubernetes clusters in kilter

Usage:
  kilter <command> [flags]

Commands:
  analyze     Instant cost & waste report from a kubeconfig (read-only)
  agent       Collect snapshots and ship them to a brain
  brain       Run the central decision service
  controller  Execute brain plans (dry-run by default)
  plan        Fetch and print the current plan from a brain
  simulate    Replay a snapshot file through the decision engine
  version     Print version

Run "kilter <command> -h" for command flags.
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Print(rootUsage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "analyze":
		err = runAnalyze(args)
	case "agent":
		err = runAgent(args)
	case "brain":
		err = runBrain(args)
	case "controller":
		err = runController(args)
	case "plan":
		err = runPlanCmd(args)
	case "simulate":
		err = runSimulate(args)
	case "version", "--version", "-v":
		fmt.Printf("kilter %s (%s)\n", version, commit)
	case "help", "-h", "--help":
		fmt.Print(rootUsage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, rootUsage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "kilter %s: %v\n", cmd, err)
		os.Exit(1)
	}
}
