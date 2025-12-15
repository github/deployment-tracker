package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

var tmplNS = "{{namespace}}"
var tmplDN = "{{deploymentName}}"
var tmplCN = "{{containerName}}"
var defaultTemplate = tmplNS + "/" + tmplDN + "/" + tmplCN

// Config holds the global configuration for the controller.
type Config struct {
	Template            string
	LogicalEnvironment  string
	PhysicalEnvironment string
	Cluster             string
	APIToken            string
	BaseURL             string
	Org                 string
}

// PodEvent represents a pod event to be processed.
type PodEvent struct {
	Key       string
	EventType string
	Pod       *corev1.Pod
}

// Controller is the Kubernetes controller for tracking deployments.
type Controller struct {
	clientset   kubernetes.Interface
	podInformer cache.SharedIndexInformer
	workqueue   workqueue.TypedRateLimitingInterface[PodEvent]
	apiClient   *deploymentrecord.Client
	cfg         *Config
}

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

	var cfg = Config{
		Template:            getEnvOrDefault("DN_TEMPLATE", defaultTemplate),
		LogicalEnvironment:  os.Getenv("LOGICAL_ENVIRONMENT"),
		PhysicalEnvironment: os.Getenv("PHYSICAL_ENVIRONMENT"),
		Cluster:             os.Getenv("CLUSTER"),
		APIToken:            getEnvOrDefault("API_TOKEN", ""),
		BaseURL:             getEnvOrDefault("BASE_URL", "api.github.com"),
		Org:                 os.Getenv("ORG"),
	}

	if cfg.LogicalEnvironment == "" {
		fmt.Fprint(os.Stderr, "Logical environment is required\n")
		os.Exit(1)
	}
	if cfg.Cluster == "" {
		fmt.Fprint(os.Stderr, "Cluster is required\n")
		os.Exit(1)
	}
	if cfg.Org == "" {
		fmt.Fprint(os.Stderr, "Org is required\n")
		os.Exit(1)
	}

	config, err := createConfig(kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating Kubernetes config: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
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

	controller := NewController(clientset, namespace, &cfg)

	fmt.Println("Starting deployment-tracker controller")
	if err := controller.Run(ctx, workers); err != nil {
		fmt.Fprintf(os.Stderr, "Error running controller: %v\n", err)
		cancel()
		os.Exit(1)
	}
	cancel()
}

func createConfig(kubeconfig string) (*rest.Config, error) {
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

// NewController creates a new deployment tracker controller.
func NewController(clientset kubernetes.Interface, namespace string, cfg *Config) *Controller {
	// Create informer factory
	var factory informers.SharedInformerFactory
	if namespace == "" {
		factory = informers.NewSharedInformerFactory(clientset, 30*time.Second)
	} else {
		factory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Second,
			informers.WithNamespace(namespace),
		)
	}

	podInformer := factory.Core().V1().Pods().Informer()

	// Create work queue with rate limiting
	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[PodEvent](),
	)

	// Create API client with optional token
	clientOpts := []deploymentrecord.ClientOption{}
	if cfg.APIToken != "" {
		clientOpts = append(clientOpts, deploymentrecord.WithAPIToken(cfg.APIToken))
	}
	apiClient := deploymentrecord.NewClient(cfg.BaseURL, cfg.Org, clientOpts...)

	controller := &Controller{
		clientset:   clientset,
		podInformer: podInformer,
		workqueue:   queue,
		apiClient:   apiClient,
		cfg:         cfg,
	}

	// Add event handlers to the informer
	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				fmt.Printf("error: invalid object returned: %+v\n",
					obj)
				return
			}

			// Only process pods that are running
			if pod.Status.Phase == corev1.PodRunning {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					queue.Add(PodEvent{Key: key, EventType: "CREATED", Pod: pod.DeepCopy()})
				}
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, ok := oldObj.(*corev1.Pod)
			if !ok {
				fmt.Printf("error: invalid object returned: %+v\n",
					oldObj)
				return
			}
			newPod, ok := newObj.(*corev1.Pod)
			if !ok {
				fmt.Printf("error: invalid object returned: %+v\n",
					newObj)
				return
			}

			// Skip if pod is being deleted
			if newPod.DeletionTimestamp != nil {
				return
			}

			// Only process if pod just became running
			if oldPod.Status.Phase != corev1.PodRunning && newPod.Status.Phase == corev1.PodRunning {
				key, err := cache.MetaNamespaceKeyFunc(newObj)
				if err == nil {
					queue.Add(PodEvent{Key: key, EventType: "CREATED", Pod: newPod.DeepCopy()})
				}
			}
		},
		DeleteFunc: func(obj any) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				// Handle deleted final state unknown
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				pod, ok = tombstone.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(PodEvent{Key: key, EventType: "DELETED", Pod: pod.DeepCopy()})
			}
		},
	})

	if err != nil {
		fmt.Printf("ERROR: failed to add event handlers: %s\n", err)
	}

	return controller
}

// Run starts the controller.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	fmt.Println("Starting pod informer")

	// Start the informer
	go c.podInformer.Run(ctx.Done())

	// Wait for the cache to be synced
	fmt.Println("Waiting for informer cache to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.podInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}

	fmt.Printf("Starting %d workers\n", workers)

	// Start workers
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	fmt.Println("Controller started")

	<-ctx.Done()
	fmt.Println("Shutting down workers")

	return nil
}

// runWorker runs a worker to process items from the work queue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextItem(ctx) {
	}
}

// processNextItem processes the next item from the work queue.
func (c *Controller) processNextItem(ctx context.Context) bool {
	event, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(event)

	err := c.processEvent(ctx, event)
	if err == nil {
		c.workqueue.Forget(event)
		return true
	}

	// Requeue on error with rate limiting
	fmt.Printf("Error processing %s: %v, requeuing\n", event.Key, err)
	c.workqueue.AddRateLimited(event)

	return true
}

// processEvent processes a single pod event.
func (c *Controller) processEvent(ctx context.Context, event PodEvent) error {
	timestamp := time.Now().Format(time.RFC3339)

	pod := event.Pod
	if pod == nil {
		return nil
	}

	status := deploymentrecord.StatusDeployed
	if event.EventType == "DELETED" {
		status = deploymentrecord.StatusDecommissioned
	}

	var lastErr error

	// Record info for each container in the pod
	for _, container := range pod.Spec.Containers {
		if err := c.recordContainer(ctx, pod, container, status, event.EventType, timestamp); err != nil {
			lastErr = err
		}
	}

	// Also record init containers
	for _, container := range pod.Spec.InitContainers {
		if err := c.recordContainer(ctx, pod, container, status, event.EventType, timestamp); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// recordContainer records a single container's deployment info.
func (c *Controller) recordContainer(ctx context.Context, pod *corev1.Pod, container corev1.Container, status, eventType, timestamp string) error {
	dn := getARDeploymentName(pod, container, c.cfg.Template)
	digest := getContainerDigest(pod, container.Name)

	if dn == "" || digest == "" {
		return nil
	}

	// Extract image name and tag
	imageName, version := deploymentrecord.ExtractImageName(container.Image)

	// Create deployment record
	record := deploymentrecord.NewDeploymentRecord(
		imageName,
		digest,
		version,
		c.cfg.LogicalEnvironment,
		c.cfg.PhysicalEnvironment,
		c.cfg.Cluster,
		status,
		dn,
	)

	if err := c.apiClient.PostOne(ctx, record); err != nil {
		fmt.Printf("[%s] FAILED %s name=%s deployment_name=%s error=%v\n",
			timestamp, eventType, record.Name, record.DeploymentName, err)
		return err
	}

	fmt.Printf("[%s] OK %s name=%s deployment_name=%s digest=%s status=%s\n",
		timestamp, eventType, record.Name, record.DeploymentName, record.Digest, record.Status)
	return nil
}

// getARDeploymentName converts the pod's metadata into the correct format
// for the deployment name for the artifact registry (this is not the same
// as the K8s deployment's name!
// The deployent name must unique within logical, physical environment and
// the cluster.
func getARDeploymentName(p *corev1.Pod, c corev1.Container, tmpl string) string {
	res := tmpl
	res = strings.ReplaceAll(res, tmplNS, p.Namespace)
	res = strings.ReplaceAll(res, tmplDN, getDeploymentName(p))
	res = strings.ReplaceAll(res, tmplCN, c.Name)
	return res
}

// getContainerDigest extracts the image digest from the container status.
func getContainerDigest(pod *corev1.Pod, containerName string) string {
	// Check regular container statuses
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			return extractDigest(status.ImageID)
		}
	}

	// Check init container statuses
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == containerName {
			return extractDigest(status.ImageID)
		}
	}

	return ""
}

// extractDigest extracts the digest from an ImageID.
// ImageID format is typically: docker-pullable://image@sha256:abc123...
// or docker://sha256:abc123...
func extractDigest(imageID string) string {
	if imageID == "" {
		return ""
	}

	// Look for sha256: in the imageID
	for i := 0; i < len(imageID)-7; i++ {
		if imageID[i:i+7] == "sha256:" {
			// Return everything from sha256: onwards
			remaining := imageID[i:]
			// Find end (could be end of string or next separator)
			end := len(remaining)
			for j, c := range remaining {
				if c == '@' || c == ' ' {
					end = j
					break
				}
			}
			return remaining[:end]
		}
	}

	return imageID
}

// getDeploymentName returns the deployment name for a pod, if it belongs
// to one.
func getDeploymentName(pod *corev1.Pod) string {
	// Pods created by Deployments are owned by ReplicaSets
	// The ReplicaSet name follows the pattern: <deployment-name>-<hash>
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" {
			// Extract deployment name by removing the hash suffix
			// ReplicaSet name format: <deployment-name>-<hash>
			rsName := owner.Name
			lastDash := strings.LastIndex(rsName, "-")
			if lastDash > 0 {
				return rsName[:lastDash]
			}
			return rsName
		}
	}
	return ""
}
