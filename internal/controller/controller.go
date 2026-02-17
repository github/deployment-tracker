package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/github/deployment-tracker/pkg/image"
	"github.com/github/deployment-tracker/pkg/metrics"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	amcache "k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/client-go/metadata"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// EventCreated indicates that a pod has been created.
	EventCreated = "CREATED"
	// EventDeleted indicates that a pod has been deleted.
	EventDeleted = "DELETED"
	// RuntimeRiskAnnotationKey represents the annotation key for runtime risks.
	RuntimeRiskAnnotationKey = "github.com/runtime-risks"
)

type ttlCache interface {
	Get(k any) (any, bool)
	Set(k any, v any, ttl time.Duration)
	Delete(k any)
}

// PodEvent represents a pod event to be processed.
type PodEvent struct {
	Key        string
	EventType  string
	DeletedPod *corev1.Pod // Only populated for delete events
}

// AggregatePodMetadata represents combined metadata for a pod and its ownership hierarchy.
type AggregatePodMetadata struct {
	RuntimeRisks map[deploymentrecord.RuntimeRisk]bool
}

// Controller is the Kubernetes controller for tracking deployments.
type Controller struct {
	clientset      kubernetes.Interface
	metadataClient metadata.Interface
	podInformer    cache.SharedIndexInformer
	workqueue      workqueue.TypedRateLimitingInterface[PodEvent]
	apiClient      *deploymentrecord.Client
	cfg            *Config
	// best effort cache to avoid redundant posts
	// post requests are idempotent, so if this cache fails due to
	// restarts or other events, nothing will break.
	observedDeployments ttlCache
}

// New creates a new deployment tracker controller.
func New(clientset kubernetes.Interface, metadataClient metadata.Interface, namespace string, excludeNamespaces string, cfg *Config) (*Controller, error) {
	// Create informer factory
	factory := createInformerFactory(clientset, namespace, excludeNamespaces)

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
	if cfg.GHAppID != "" &&
		cfg.GHInstallID != "" &&
		cfg.GHAppPrivateKey != "" {
		clientOpts = append(clientOpts, deploymentrecord.WithGHApp(cfg.GHAppID, cfg.GHInstallID, cfg.GHAppPrivateKey))
	}

	apiClient, err := deploymentrecord.NewClient(
		cfg.BaseURL,
		cfg.Organization,
		clientOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create API client: %w", err)
	}

	cntrl := &Controller{
		clientset:           clientset,
		metadataClient:      metadataClient,
		podInformer:         podInformer,
		workqueue:           queue,
		apiClient:           apiClient,
		cfg:                 cfg,
		observedDeployments: amcache.NewExpiring(),
	}

	// Add event handlers to the informer
	_, err = podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				slog.Error("Invalid object returned",
					"object", obj,
				)
				return
			}

			// Only process pods that are running and belong
			// to a deployment
			if pod.Status.Phase == corev1.PodRunning && getDeploymentName(pod) != "" {
				key, err := cache.MetaNamespaceKeyFunc(obj)

				// For our purposes, there are in practice
				// no error event we care about, so don't
				// bother with handling it.
				if err == nil {
					queue.Add(PodEvent{
						Key:       key,
						EventType: EventCreated,
					})
				}
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, ok := oldObj.(*corev1.Pod)
			if !ok {
				slog.Error("Invalid old object returned",
					"object", oldObj,
				)
				return
			}
			newPod, ok := newObj.(*corev1.Pod)
			if !ok {
				slog.Error("Invalid new object returned",
					"object", newObj,
				)
				return
			}

			// Skip if pod is being deleted or doesn't belong
			// to a deployment
			if newPod.DeletionTimestamp != nil || getDeploymentName(newPod) == "" {
				return
			}

			// Only process if pod just became running.
			// We need to process this as often when a container
			// is created, the spec does not contain the digest
			// so we need to wait for the status field to be
			// populated from where we can get the digest.
			if oldPod.Status.Phase != corev1.PodRunning &&
				newPod.Status.Phase == corev1.PodRunning {
				key, err := cache.MetaNamespaceKeyFunc(newObj)

				// For our purposes, there are in practice
				// no error event we care about, so don't
				// bother with handling it.
				if err == nil {
					queue.Add(PodEvent{
						Key:       key,
						EventType: EventCreated,
					})
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

			// Only process pods that belong to a deployment
			if getDeploymentName(pod) == "" {
				return
			}

			key, err := cache.MetaNamespaceKeyFunc(obj)
			// For our purposes, there are in practice
			// no error event we care about, so don't
			// bother with handling it.
			if err == nil {
				queue.Add(PodEvent{
					Key:        key,
					EventType:  EventDeleted,
					DeletedPod: pod,
				})
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handlers: %w", err)
	}

	return cntrl, nil
}

// Run starts the controller.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	slog.Info("Starting pod informer")

	// Start the informer
	go c.podInformer.Run(ctx.Done())

	// Wait for the cache to be synced
	slog.Info("Waiting for informer cache to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.podInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}

	slog.Info("Starting workers",
		"count", workers,
	)

	// Start workers
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	slog.Info("Controller started")

	<-ctx.Done()
	slog.Info("Shutting down workers")

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

	start := time.Now()
	err := c.processEvent(ctx, event)
	dur := time.Since(start)

	if err == nil {
		metrics.EventsProcessedOk.WithLabelValues(event.EventType).Inc()
		metrics.EventsProcessedTimer.WithLabelValues("ok").Observe(dur.Seconds())

		c.workqueue.Forget(event)
		return true
	}
	metrics.EventsProcessedTimer.WithLabelValues("failed").Observe(dur.Seconds())
	metrics.EventsProcessedFailed.WithLabelValues(event.EventType).Inc()

	// Requeue on error with rate limiting
	slog.Error("Failed to process event, requeuing",
		"event_key", event.Key,
		"error", err,
	)
	c.workqueue.AddRateLimited(event)

	return true
}

// processEvent processes a single pod event.
func (c *Controller) processEvent(ctx context.Context, event PodEvent) error {
	var pod *corev1.Pod

	if event.EventType == EventDeleted {
		// For delete events, use the pod captured at deletion time
		pod = event.DeletedPod
		if pod == nil {
			slog.Error("Delete event missing pod data",
				"key", event.Key,
			)
			return nil
		}

		// Check if the parent deployment still exists
		// If it does, this is just a scale-down event, skip it.
		//
		// If a deployment changes image versions, this will not
		// fire delete/decommissioned events to the remote API.
		// This is as intended, as the server will keep track of
		// the (cluster unique) deployment name, and just update
		// the referenced image digest to the newly observed (via
		// the create event).
		deploymentName := getDeploymentName(pod)
		if deploymentName != "" && c.deploymentExists(ctx, pod.Namespace, deploymentName) {
			slog.Debug("Deployment still exists, skipping pod delete (scale down)",
				"namespace", pod.Namespace,
				"deployment", deploymentName,
				"pod", pod.Name,
			)
			return nil
		}
	} else {
		// For create events, get the pod from the informer's cache
		obj, exists, err := c.podInformer.GetIndexer().GetByKey(event.Key)
		if err != nil {
			slog.Error("Failed to get pod from cache",
				"key", event.Key,
				"error", err,
			)
			return nil
		}
		if !exists {
			// Pod no longer exists in cache, skip processing
			return nil
		}

		var ok bool
		pod, ok = obj.(*corev1.Pod)
		if !ok {
			slog.Error("Invalid object type in cache",
				"key", event.Key,
			)
			return nil
		}
	}

	status := deploymentrecord.StatusDeployed
	if event.EventType == EventDeleted {
		status = deploymentrecord.StatusDecommissioned
	}

	var lastErr error

	// Gather aggregate metadata for adds/updates
	var runtimeRisks []deploymentrecord.RuntimeRisk
	if status != deploymentrecord.StatusDecommissioned {
		aggMetadata := c.aggregateMetadata(ctx, podToPartialMetadata(pod))
		for risk := range aggMetadata.RuntimeRisks {
			runtimeRisks = append(runtimeRisks, risk)
		}
		slices.Sort(runtimeRisks)
	}

	// Record info for each container in the pod
	for _, container := range pod.Spec.Containers {
		if err := c.recordContainer(ctx, pod, container, status, event.EventType, runtimeRisks); err != nil {
			lastErr = err
		}
	}

	// Also record init containers
	for _, container := range pod.Spec.InitContainers {
		if err := c.recordContainer(ctx, pod, container, status, event.EventType, runtimeRisks); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// deploymentExists checks if a deployment exists in the cluster.
func (c *Controller) deploymentExists(ctx context.Context, namespace, name string) bool {
	_, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		// On error, assume it exists to be safe
		// (avoid false decommissions)
		slog.Warn("Failed to check if deployment exists, assuming it does",
			"namespace", namespace,
			"deployment", name,
			"error", err,
		)
		return true
	}
	return true
}

// recordContainer records a single container's deployment info.
func (c *Controller) recordContainer(ctx context.Context, pod *corev1.Pod, container corev1.Container, status, eventType string, runtimeRisks []deploymentrecord.RuntimeRisk) error {
	var cacheKey string

	dn := getARDeploymentName(pod, container, c.cfg.Template)
	digest := getContainerDigest(pod, container.Name)

	if dn == "" || digest == "" {
		slog.Debug("Skipping container: missing deployment name or digest",
			"namespace", pod.Namespace,
			"pod", pod.Name,
			"container", container.Name,
			"deployment_name", dn,
			"has_digest", digest != "",
		)
		return nil
	}

	// Check if we've already recorded this deployment
	switch status {
	case deploymentrecord.StatusDeployed:
		cacheKey = getCacheKey(EventCreated, dn, digest)
		if _, exists := c.observedDeployments.Get(cacheKey); exists {
			slog.Debug("Deployment already observed, skipping post",
				"deployment_name", dn,
				"digest", digest,
			)
			return nil
		}
	case deploymentrecord.StatusDecommissioned:
		cacheKey = getCacheKey(EventDeleted, dn, digest)
		if _, exists := c.observedDeployments.Get(cacheKey); exists {
			slog.Debug("Deployment already deleted, skipping post",
				"deployment_name", dn,
				"digest", digest,
			)
			return nil
		}
	default:
		return fmt.Errorf("invalid status: %s", status)
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
		runtimeRisks,
	)

	if err := c.apiClient.PostOne(ctx, record); err != nil {
		// Make sure to not retry on client error messages
		var clientErr *deploymentrecord.ClientError
		if errors.As(err, &clientErr) {
			slog.Warn("Failed to post record",
				"event_type", eventType,
				"name", record.Name,
				"deployment_name", record.DeploymentName,
				"status", record.Status,
				"digest", record.Digest,
				"error", err,
			)
			return nil
		}

		slog.Error("Failed to post record",
			"event_type", eventType,
			"name", record.Name,
			"deployment_name", record.DeploymentName,
			"status", record.Status,
			"digest", record.Digest,
			"error", err,
		)
		return err
	}

	slog.Info("Posted record",
		"event_type", eventType,
		"name", record.Name,
		"deployment_name", record.DeploymentName,
		"status", record.Status,
		"runtime_risks", record.RuntimeRisks,
		"digest", record.Digest,
	)

	// Update cache after successful post
	switch status {
	case deploymentrecord.StatusDeployed:
		cacheKey = getCacheKey(EventCreated, dn, digest)
		c.observedDeployments.Set(cacheKey, true, 2*time.Minute)
		// If there was a previous delete event, remove that
		cacheKey = getCacheKey(EventDeleted, dn, digest)
		c.observedDeployments.Delete(cacheKey)
	case deploymentrecord.StatusDecommissioned:
		cacheKey = getCacheKey(EventDeleted, dn, digest)
		c.observedDeployments.Set(cacheKey, true, 2*time.Minute)
		// If there was a previous created event, remove that
		cacheKey = getCacheKey(EventCreated, dn, digest)
		c.observedDeployments.Delete(cacheKey)
	default:
		return fmt.Errorf("invalid status: %s", status)
	}

	return nil
}

// aggregateMetadata returns aggregated metadata for a pod and its owners.
func (c *Controller) aggregateMetadata(ctx context.Context, obj *metav1.PartialObjectMetadata) AggregatePodMetadata {
	aggMetadata := AggregatePodMetadata{
		RuntimeRisks: make(map[deploymentrecord.RuntimeRisk]bool),
	}
	queue := []*metav1.PartialObjectMetadata{obj}
	visited := make(map[types.UID]bool)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if visited[current.GetUID()] {
			slog.Warn("Already visited object, skipping to avoid cycles",
				"UID", current.GetUID(),
				"name", current.GetName(),
			)
			continue
		}
		visited[current.GetUID()] = true

		extractMetadataFromObject(current, &aggMetadata)
		c.addOwnersToQueue(ctx, current, &queue)
	}

	return aggMetadata
}

// addOwnersToQueue takes a current object and looks up its owners, adding them to the queue for processing
// to collect their metadata.
func (c *Controller) addOwnersToQueue(ctx context.Context, current *metav1.PartialObjectMetadata, queue *[]*metav1.PartialObjectMetadata) {
	ownerRefs := current.GetOwnerReferences()

	for _, owner := range ownerRefs {
		ownerObj, err := c.getOwnerMetadata(ctx, current.GetNamespace(), owner)
		if err != nil {
			slog.Warn("Failed to get owner object for metadata collection",
				"namespace", current.GetNamespace(),
				"owner_kind", owner.Kind,
				"owner_name", owner.Name,
				"error", err,
			)
			continue
		}

		if ownerObj == nil {
			continue
		}

		*queue = append(*queue, ownerObj)
	}
}

// getOwnerMetadata retrieves partial object metadata for an owner ref.
func (c *Controller) getOwnerMetadata(ctx context.Context, namespace string, owner metav1.OwnerReference) (*metav1.PartialObjectMetadata, error) {
	gvr := schema.GroupVersionResource{
		Group:   "apps",
		Version: "v1",
	}

	switch owner.Kind {
	case "ReplicaSet":
		gvr.Resource = "replicasets"
	case "Deployment":
		gvr.Resource = "deployments"
	default:
		slog.Debug("Unsupported owner kind for runtime risk collection",
			"kind", owner.Kind,
			"name", owner.Name,
		)
		return nil, nil
	}

	obj, err := c.metadataClient.Resource(gvr).Namespace(namespace).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			slog.Debug("Owner object not found for metadata collection",
				"namespace", namespace,
				"owner_kind", owner.Kind,
				"owner_name", owner.Name,
			)
			return nil, nil
		}
		return nil, err
	}
	return obj, nil
}

func getCacheKey(ev, dn, digest string) string {
	return ev + "||" + dn + "||" + digest
}

// createInformerFactory creates a shared informer factory with the given resync period.
// If excludeNamespaces is non-empty, it will exclude those namespaces from being watched.
// If namespace is non-empty, it will only watch that namespace.
func createInformerFactory(clientset kubernetes.Interface, namespace string, excludeNamespaces string) informers.SharedInformerFactory {
	var factory informers.SharedInformerFactory
	switch {
	case namespace != "":
		slog.Info("Namespace to watch",
			"namespace",
			namespace,
		)
		factory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Second,
			informers.WithNamespace(namespace),
		)
	case excludeNamespaces != "":
		seenNamespaces := make(map[string]bool)
		fieldSelectorParts := make([]string, 0)

		for _, ns := range strings.Split(excludeNamespaces, ",") {
			ns = strings.TrimSpace(ns)
			if ns != "" && !seenNamespaces[ns] {
				seenNamespaces[ns] = true
				fieldSelectorParts = append(fieldSelectorParts, fmt.Sprintf("metadata.namespace!=%s", ns))
			}
		}

		slog.Info("Excluding namespaces from watch",
			"field_selector",
			strings.Join(fieldSelectorParts, ","),
		)
		tweakListOptions := func(options *metav1.ListOptions) {
			options.FieldSelector = strings.Join(fieldSelectorParts, ",")
		}

		factory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Second,
			informers.WithTweakListOptions(tweakListOptions),
		)
	default:
		factory = informers.NewSharedInformerFactory(clientset,
			30*time.Second,
		)
	}

	return factory
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
// The spec only contains the desired state, so any resolved digests must
// be pulled from the status field.
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

// extractMetadataFromObject extracts metadata from an object.
func extractMetadataFromObject(obj *metav1.PartialObjectMetadata, aggMetadata *AggregatePodMetadata) {
	annotations := obj.GetAnnotations()
	if risks, exists := annotations[RuntimeRiskAnnotationKey]; exists {
		for _, risk := range strings.Split(risks, ",") {
			r := deploymentrecord.ValidateRuntimeRisk(risk)
			if r != "" {
				aggMetadata.RuntimeRisks[r] = true
			}
		}
	}
}

func podToPartialMetadata(pod *corev1.Pod) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: pod.ObjectMeta,
	}
}
