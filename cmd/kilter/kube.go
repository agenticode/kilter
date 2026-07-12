package main

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// kubeClients builds core + metrics clients from --kubeconfig, $KUBECONFIG,
// ~/.kube/config, or in-cluster config, in that order.
func kubeClients(kubeconfig string) (kubernetes.Interface, metricsclient.Interface, error) {
	cfg, err := restConfig(kubeconfig)
	if err != nil {
		return nil, nil, err
	}
	// Snapshot collection is bursty LISTs; give client-side throttling room.
	cfg.QPS = 50
	cfg.Burst = 100
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	metrics, err := metricsclient.NewForConfig(cfg)
	if err != nil {
		return core, nil, nil // metrics are optional
	}
	return core, metrics, nil
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	}
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
		}
		return cfg, nil
	}
	// In-cluster first (agent/controller pods), then default kubeconfig.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no kubeconfig found (tried in-cluster, %s): %w", rules.GetDefaultFilename(), err)
	}
	return cfg, nil
}
