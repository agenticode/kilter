package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/agenticode/kilter/pkg/pricing/awssync"
)

func runPricing(args []string) error {
	if len(args) < 1 || args[0] != "sync-aws" {
		fmt.Fprintln(os.Stderr, "usage: kilter pricing sync-aws --region <region> [--families m5,c6i] [--out catalog.json]")
		return fmt.Errorf("unknown pricing subcommand")
	}
	fs := flag.NewFlagSet("pricing sync-aws", flag.ExitOnError)
	region := fs.String("region", envOr("AWS_REGION", ""), "AWS region to price (required)")
	families := fs.String("families", "", "comma-separated instance families to include (default: all)")
	out := fs.String("out", "catalog.json", "output catalog path")
	fs.Parse(args[1:])

	if *region == "" {
		return fmt.Errorf("--region is required")
	}
	var fams []string
	if *families != "" {
		for _, f := range splitComma(*families) {
			fams = append(fams, f)
		}
	}
	ctx, cancel := signalContext()
	defer cancel()
	s, err := awssync.New(ctx, *region, fams)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "syncing AWS %s prices (on-demand + spot)…\n", *region)
	raw, err := s.Sync(ctx)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, raw, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s — use it with: kilter brain --catalog %s (or analyze --catalog)\n", *out, *out)
	return nil
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if f := s[start:i]; f != "" {
				out = append(out, f)
			}
			start = i + 1
		}
	}
	return out
}
