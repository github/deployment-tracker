package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/github/deployment-tracker/internal/controller"
	"github.com/github/deployment-tracker/internal/metadata"
	k8smetadata "k8s.io/client-go/metadata"

	"github.com/prometheus/client_golang/prometheus/promhttp"
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
		kubeconfig        string
		namespace         string
		excludeNamespaces string
		workers           int
		metricsPort       string
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig file (uses in-cluster config if not set)")
	flag.StringVar(&namespace, "namespace", "", "namespace to monitor (empty for all namespaces)")
	flag.StringVar(&excludeNamespaces, "exclude-namespaces", "", "comma separated list of namespaces to exclude from monitoring (empty to include all namespaces)")
	flag.IntVar(&workers, "workers", 2, "number of worker goroutines")
	flag.StringVar(&metricsPort, "metrics-port", "9090", "port to listen to for metrics")
	flag.Parse()

	// Cannot use both
	if namespace != "" && excludeNamespaces != "" {
		slog.Error("Cannot set both -namespace and -exclude-namespaces")
		os.Exit(1)
	}

	// Validate worker count
	if workers < 1 || workers > 100 {
		slog.Error("Invalid worker count, must be between 1 and 100",
			"workers", workers)
		os.Exit(1)
	}

	// init logging
	log.SetFlags(log.LstdFlags | log.Lshortfile | log.LUTC)
	opts := slog.HandlerOptions{Level: slog.LevelInfo}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &opts)))

	var cntrlCfg = controller.Config{
		Template:            getEnvOrDefault("DN_TEMPLATE", defaultTemplate),
		LogicalEnvironment:  os.Getenv("LOGICAL_ENVIRONMENT"),
		PhysicalEnvironment: os.Getenv("PHYSICAL_ENVIRONMENT"),
		Cluster:             os.Getenv("CLUSTER"),
		APIToken:            getEnvOrDefault("API_TOKEN", ""),
		BaseURL:             getEnvOrDefault("BASE_URL", "api.github.com"),
		GHAppID:             getEnvOrDefault("GH_APP_ID", ""),
		GHInstallID:         getEnvOrDefault("GH_INSTALL_ID", ""),
		GHAppPrivateKey:     getEnvOrDefault("GH_APP_PRIV_KEY", ""),
		Organization:        os.Getenv("GITHUB_ORG"),
	}

	if !controller.ValidTemplate(cntrlCfg.Template) {
		slog.Error("Template must contain at least one placeholder",
			"template", cntrlCfg.Template,
			"valid_placeholders", []string{controller.TmplNS, controller.TmplDN, controller.TmplCN})
		os.Exit(1)
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
		slog.Error("Organization is required")
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

	// Create metadata client
	metadataClient, err := k8smetadata.NewForConfig(k8sCfg)
	if err != nil {
		slog.Error("Error creating Kubernetes metadata client",
			"error", err)
		os.Exit(1)
	}

	// Create metadata aggregator
	metadataAggregator := metadata.NewAggregator(metadataClient)

	// Start the metrics server
	var promSrv = &http.Server{
		Addr:              ":" + metricsPort,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		Handler:           http.NewServeMux(),
	}
	promSrv.Handler.(*http.ServeMux).Handle("/metrics", promhttp.Handler())

	go func() {
		slog.Info("starting Prometheus metrics server",
			"url", promSrv.Addr)
		if err := promSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("failed to start metrics server",
				"error", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("Shutting down...")

		// Gracefully shutdown the metrics server
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := promSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("failed to shutdown metrics server gracefully",
				"error", err)
		}

		cancel()
	}()

	cntrl, err := controller.New(clientset, metadataAggregator, namespace, excludeNamespaces, &cntrlCfg)
	if err != nil {
		slog.Error("Failed to create controller",
			"error", err)
		os.Exit(1)
	}

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
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home directory: %w", err)
	}
	return clientcmd.BuildConfigFromFlags("", homeDir+"/.kube/config")
}
