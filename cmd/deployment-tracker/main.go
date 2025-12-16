package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/github/deployment-tracker/internal/controller"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var defaultTemplate = controller.TmplNS + "/" +
	controller.TmplDN + "/" +
	controller.TmplCN

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	var (
		kubeconfig string
		namespace  string
		workers    int
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig file (uses in-cluster config if not set)")
	flag.StringVar(&namespace, "namespace", "", "namespace to monitor (empty for all namespaces)")
	flag.IntVar(&workers, "workers", 2, "number of worker goroutines")
	flag.Parse()

	// init logging
	opts := slog.HandlerOptions{Level: slog.LevelInfo}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &opts)))

	var cntrlCfg = controller.Config{
		Template:            getEnvOrDefault("DN_TEMPLATE", defaultTemplate),
		LogicalEnvironment:  os.Getenv("LOGICAL_ENVIRONMENT"),
		PhysicalEnvironment: os.Getenv("PHYSICAL_ENVIRONMENT"),
		Cluster:             os.Getenv("CLUSTER"),
		APIToken:            getEnvOrDefault("API_TOKEN", ""),
		BaseURL:             getEnvOrDefault("BASE_URL", "api.github.com"),
		Organization:        os.Getenv("GITHUB_ORG"),
	}

	if cntrlCfg.LogicalEnvironment == "" {
		slog.Error("Logical environment is required")
		os.Exit(1)
	}
	if cntrlCfg.Cluster == "" {
		slog.Error("Cluster is required")
		os.Exit(1)
	}
	if cntrlCfg.Organization == "" {
		slog.Error("Organiation is required")
		os.Exit(1)
	}

	k8sCfg, err := createK8sConfig(kubeconfig)
	if err != nil {
		slog.Error("Failed to create Kubernetes config",
			"error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		slog.Error("Error creating Kubernetes client",
			"error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("Shutting down...")
		cancel()
	}()

	cntrl := controller.New(clientset, namespace, &cntrlCfg)

	slog.Info("Starting deployment-tracker controller")
	if err := cntrl.Run(ctx, workers); err != nil {
		slog.Error("Error running controller",
			"error", err)
		cancel()
		os.Exit(1)
	}
	cancel()
}

func createK8sConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	if os.Getenv("KUBECONFIG") != "" {
		return clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	}

	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}

	// Fall back to default kubeconfig location
	homeDir, _ := os.UserHomeDir()
	return clientcmd.BuildConfigFromFlags("", homeDir+"/.kube/config")
}
