package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agenticode/kilter/pkg/api"
	"github.com/agenticode/kilter/pkg/collect"
)

// signalContext cancels on SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	brainURL := fs.String("brain-url", envOr("KILTER_BRAIN_URL", "http://kilter-brain:8180"), "brain base URL")
	token := fs.String("token", os.Getenv("KILTER_TOKEN"), "bearer token for the brain API")
	clusterID := fs.String("cluster-id", envOr("KILTER_CLUSTER_ID", "default"), "cluster name reported to the brain")
	interval := fs.Duration("interval", 60*time.Second, "snapshot interval")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: in-cluster)")
	namespace := fs.String("namespace", "", "restrict collection to one namespace")
	fs.Parse(args)

	client, metrics, err := kubeClients(*kubeconfig)
	if err != nil {
		return err
	}
	brain, err := api.NewClient(*brainURL, *token)
	if err != nil {
		return err
	}
	col := &collect.Collector{Client: client, Metrics: metrics, ClusterID: *clusterID, Namespace: *namespace}

	ctx, cancel := signalContext()
	defer cancel()
	log := slog.Default().With("component", "agent", "cluster", *clusterID)
	log.Info("agent starting", "brain", *brainURL, "interval", interval.String())

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		snap, err := col.Snapshot(ctx)
		if err != nil {
			log.Error("collect failed", "err", err)
		} else if err := brain.PushSnapshot(ctx, snap); err != nil {
			log.Error("push failed", "err", err)
		} else {
			log.Info("snapshot shipped",
				"nodes", len(snap.Nodes), "pods", len(snap.Pods), "usage", len(snap.Usage))
		}
		select {
		case <-ctx.Done():
			log.Info("agent stopping")
			return nil
		case <-ticker.C:
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
