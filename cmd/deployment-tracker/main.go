package main

import (
	"context"
	"flag"
	"fmt"
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

	var cntrlCfg = controller.Config{
		Template:            getEnvOrDefault("DN_TEMPLATE", defaultTemplate),
		LogicalEnvironment:  os.Getenv("LOGICAL_ENVIRONMENT"),
		PhysicalEnvironment: os.Getenv("PHYSICAL_ENVIRONMENT"),
		Cluster:             os.Getenv("CLUSTER"),
		APIToken:            getEnvOrDefault("API_TOKEN", ""),
		BaseURL:             getEnvOrDefault("BASE_URL", "api.github.com"),
		Org:                 os.Getenv("ORG"),
	}

	if cntrlCfg.LogicalEnvironment == "" {
		fmt.Fprint(os.Stderr, "Logical environment is required\n")
		os.Exit(1)
	}
	if cntrlCfg.Cluster == "" {
		fmt.Fprint(os.Stderr, "Cluster is required\n")
		os.Exit(1)
	}
	if cntrlCfg.Org == "" {
		fmt.Fprint(os.Stderr, "Org is required\n")
		os.Exit(1)
	}

	k8sCfg, err := createK8sConfig(kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Kubernetes config: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		cancel()
	}()

	cntrl := controller.New(clientset, namespace, &cntrlCfg)

	fmt.Println("Starting deployment-tracker controller")
	if err := cntrl.Run(ctx, workers); err != nil {
		fmt.Fprintf(os.Stderr, "Error running controller: %v\n", err)
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
