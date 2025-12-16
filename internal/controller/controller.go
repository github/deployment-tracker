package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/github/deployment-tracker/pkg/image"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

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

// New creates a new deployment tracker controller.
func New(clientset kubernetes.Interface, namespace string, cfg *Config) *Controller {
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

	cntrl := &Controller{
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

	return cntrl
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
	imageName, version := image.ExtractName(container.Image)

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
// The deployment name must unique within logical, physical environment and
// the cluster.
func getARDeploymentName(p *corev1.Pod, c corev1.Container, tmpl string) string {
	res := tmpl
	res = strings.ReplaceAll(res, TmplNS, p.Namespace)
	res = strings.ReplaceAll(res, TmplDN, getDeploymentName(p))
	res = strings.ReplaceAll(res, TmplCN, c.Name)
	return res
}

// getContainerDigest extracts the image digest from the container status.
func getContainerDigest(pod *corev1.Pod, containerName string) string {
	// Check regular container statuses
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			return image.ExtractDigest(status.ImageID)
		}
	}

	// Check init container statuses
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == containerName {
			return image.ExtractDigest(status.ImageID)
		}
	}

	return ""
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
