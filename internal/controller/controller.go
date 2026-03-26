package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/github/deployment-tracker/internal/metadata"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/github/deployment-tracker/pkg/dtmetrics"
	"github.com/github/deployment-tracker/pkg/ociutil"
	amcache "k8s.io/apimachinery/pkg/util/cache"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// EventCreated indicates that a pod has been created.
	EventCreated = "CREATED"
	// EventDeleted indicates that a pod has been deleted.
	EventDeleted = "DELETED"

	// unknownArtifactTTL is the TTL for cached 404 responses from the
	// deployment record API. Once an artifact is known to be missing,
	// we suppress further API calls for this duration.
	unknownArtifactTTL = 1 * time.Hour
)

type ttlCache interface {
	Get(k any) (any, bool)
	Set(k any, v any, ttl time.Duration)
	Delete(k any)
}

type deploymentRecordPoster interface {
	PostOne(ctx context.Context, record *deploymentrecord.DeploymentRecord) error
}

type podMetadataAggregator interface {
	BuildAggregatePodMetadata(ctx context.Context, obj *metav1.PartialObjectMetadata) *metadata.AggregatePodMetadata
}

// PodEvent represents a pod event to be processed.
type PodEvent struct {
	Key        string
	EventType  string
	DeletedPod *corev1.Pod // Only populated for delete events
}

// workloadRef describes the top-level workload that owns a pod.
type workloadRef struct {
	Name string
	Kind string // "Deployment", "DaemonSet", "StatefulSet", "CronJob", or "Job"
}

// Controller is the Kubernetes controller for tracking deployments.
type Controller struct {
	clientset           kubernetes.Interface
	metadataAggregator  podMetadataAggregator
	podInformer         cache.SharedIndexInformer
	deploymentInformer  cache.SharedIndexInformer
	deploymentLister    appslisters.DeploymentLister
	daemonSetInformer   cache.SharedIndexInformer
	daemonSetLister     appslisters.DaemonSetLister
	statefulSetInformer cache.SharedIndexInformer
	statefulSetLister   appslisters.StatefulSetLister
	jobInformer         cache.SharedIndexInformer
	jobLister           batchlisters.JobLister
	cronJobInformer     cache.SharedIndexInformer
	cronJobLister       batchlisters.CronJobLister
	workqueue           workqueue.TypedRateLimitingInterface[PodEvent]
	apiClient           deploymentRecordPoster
	cfg                 *Config
	// best effort cache to avoid redundant posts
	// post requests are idempotent, so if this cache fails due to
	// restarts or other events, nothing will break.
	observedDeployments ttlCache
	// best effort cache to suppress API calls for artifacts that
	// returned a 404 (no artifact found). Keyed by image digest.
	unknownArtifacts ttlCache
}

// New creates a new deployment tracker controller.
func New(clientset kubernetes.Interface, metadataAggregator podMetadataAggregator, namespace string, excludeNamespaces string, cfg *Config) (*Controller, error) {
	// Create informer factory
	factory := createInformerFactory(clientset, namespace, excludeNamespaces)

	podInformer := factory.Core().V1().Pods().Informer()
	deploymentInformer := factory.Apps().V1().Deployments().Informer()
	deploymentLister := factory.Apps().V1().Deployments().Lister()
	daemonSetInformer := factory.Apps().V1().DaemonSets().Informer()
	daemonSetLister := factory.Apps().V1().DaemonSets().Lister()
	statefulSetInformer := factory.Apps().V1().StatefulSets().Informer()
	statefulSetLister := factory.Apps().V1().StatefulSets().Lister()
	jobInformer := factory.Batch().V1().Jobs().Informer()
	jobLister := factory.Batch().V1().Jobs().Lister()
	cronJobInformer := factory.Batch().V1().CronJobs().Informer()
	cronJobLister := factory.Batch().V1().CronJobs().Lister()

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
		(len(cfg.GHAppPrivateKey) > 0 || cfg.GHAppPrivateKeyPath != "") {
		clientOpts = append(clientOpts, deploymentrecord.WithGHApp(
			cfg.GHAppID,
			cfg.GHInstallID,
			cfg.GHAppPrivateKey,
			cfg.GHAppPrivateKeyPath,
		))
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
		metadataAggregator:  metadataAggregator,
		podInformer:         podInformer,
		deploymentInformer:  deploymentInformer,
		deploymentLister:    deploymentLister,
		daemonSetInformer:   daemonSetInformer,
		daemonSetLister:     daemonSetLister,
		statefulSetInformer: statefulSetInformer,
		statefulSetLister:   statefulSetLister,
		jobInformer:         jobInformer,
		jobLister:           jobLister,
		cronJobInformer:     cronJobInformer,
		cronJobLister:       cronJobLister,
		workqueue:           queue,
		apiClient:           apiClient,
		cfg:                 cfg,
		observedDeployments: amcache.NewExpiring(),
		unknownArtifacts:    amcache.NewExpiring(),
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
			// to a supported workload (Deployment, DaemonSet, StatefulSet, Job, or CronJob)
			if pod.Status.Phase == corev1.PodRunning && hasSupportedOwner(pod) {
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

			// Also process Job-owned pods that completed before
			// we observed them in Running phase (e.g. sub-second Jobs).
			if isTerminalPhase(pod) && getJobOwnerName(pod) != "" {
				key, err := cache.MetaNamespaceKeyFunc(obj)
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
			// to a supported workload.
			// Exception: Job-owned pods transitioning to a terminal phase
			// (Succeeded/Failed) from a non-Running state should still be
			// processed — this catches short-lived Jobs that skip Running.
			// We exclude Running→terminal transitions since those pods
			// were already enqueued when they entered Running.
			isJobTerminal := !isTerminalPhase(oldPod) && isTerminalPhase(newPod) &&
				oldPod.Status.Phase != corev1.PodRunning && getJobOwnerName(newPod) != ""
			if !isJobTerminal {
				if newPod.DeletionTimestamp != nil || !hasSupportedOwner(newPod) {
					return
				}
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

			// Also catch Job-owned pods that transitioned directly
			// to a terminal phase without us seeing them as Running.
			if isJobTerminal {
				key, err := cache.MetaNamespaceKeyFunc(newObj)
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

			// Only process pods that belong to a supported workload
			if !hasSupportedOwner(pod) {
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

	slog.Info("Starting informers")

	// Start the informers
	go c.podInformer.Run(ctx.Done())
	go c.deploymentInformer.Run(ctx.Done())
	go c.daemonSetInformer.Run(ctx.Done())
	go c.statefulSetInformer.Run(ctx.Done())
	go c.jobInformer.Run(ctx.Done())
	go c.cronJobInformer.Run(ctx.Done())

	// Wait for the caches to be synced
	slog.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(),
		c.podInformer.HasSynced,
		c.deploymentInformer.HasSynced,
		c.daemonSetInformer.HasSynced,
		c.statefulSetInformer.HasSynced,
		c.jobInformer.HasSynced,
		c.cronJobInformer.HasSynced,
	) {
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
		dtmetrics.EventsProcessedOk.WithLabelValues(event.EventType).Inc()
		dtmetrics.EventsProcessedTimer.WithLabelValues("ok").Observe(dur.Seconds())

		c.workqueue.Forget(event)
		return true
	}
	dtmetrics.EventsProcessedTimer.WithLabelValues("failed").Observe(dur.Seconds())
	dtmetrics.EventsProcessedFailed.WithLabelValues(event.EventType).Inc()

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
	var wl workloadRef

	if event.EventType == EventDeleted {
		// For delete events, use the pod captured at deletion time
		pod = event.DeletedPod
		if pod == nil {
			slog.Error("Delete event missing pod data",
				"key", event.Key,
			)
			return nil
		}

		// Check if the parent workload still exists.
		// If it does, this is just a scale-down event (or a completed
		// Job pod while the CronJob is still active), skip it.
		//
		// If a workload changes image versions, this will not
		// fire delete/decommissioned events to the remote API.
		// This is as intended, as the server will keep track of
		// the (cluster unique) deployment name, and just update
		// the referenced image digest to the newly observed (via
		// the create event).
		wl = c.getWorkloadRef(pod)
		if wl.Name != "" && c.workloadActive(pod.Namespace, wl) {
			slog.Debug("Parent workload still exists, skipping pod delete",
				"namespace", pod.Namespace,
				"workload_kind", wl.Kind,
				"workload_name", wl.Name,
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

	// Resolve the workload name for the deployment record.
	// For delete events, wl was already resolved above.
	if wl.Name == "" {
		wl = c.getWorkloadRef(pod)
	}
	if wl.Name == "" {
		slog.Debug("Could not resolve workload name for pod, skipping",
			"namespace", pod.Namespace,
			"pod", pod.Name,
		)
		return nil
	}

	var lastErr error

	// Gather aggregate metadata for adds/updates
	var aggPodMetadata *metadata.AggregatePodMetadata
	if event.EventType != EventDeleted {
		aggPodMetadata = c.metadataAggregator.BuildAggregatePodMetadata(ctx, podToPartialMetadata(pod))
	}

	// Record info for each container in the pod
	for _, container := range pod.Spec.Containers {
		if err := c.recordContainer(ctx, pod, container, event.EventType, wl.Name, aggPodMetadata); err != nil {
			lastErr = err
		}
	}

	// Also record init containers
	for _, container := range pod.Spec.InitContainers {
		if err := c.recordContainer(ctx, pod, container, event.EventType, wl.Name, aggPodMetadata); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// deploymentExists checks if a deployment exists in the local informer cache.
func (c *Controller) deploymentExists(namespace, name string) bool {
	_, err := c.deploymentLister.Deployments(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if deployment exists in cache, assuming it does",
			"namespace", namespace,
			"deployment", name,
			"error", err,
		)
		return true
	}
	return true
}

// recordContainer records a single container's deployment info.
func (c *Controller) recordContainer(ctx context.Context, pod *corev1.Pod, container corev1.Container, eventType, workloadName string, aggPodMetadata *metadata.AggregatePodMetadata) error {
	var cacheKey string

	status := deploymentrecord.StatusDeployed
	if eventType == EventDeleted {
		status = deploymentrecord.StatusDecommissioned
	}

	dn := getARDeploymentName(pod, container, c.cfg.Template, workloadName)
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

	// Check if this artifact was previously unknown (404 from the API)
	if _, exists := c.unknownArtifacts.Get(digest); exists {
		dtmetrics.PostDeploymentRecordUnknownArtifactCacheHit.Inc()
		slog.Debug("Artifact previously returned 404, skipping post",
			"deployment_name", dn,
			"digest", digest,
		)
		return nil
	}

	// Extract image name and tag
	imageName, version := ociutil.ExtractName(container.Image)

	// Format runtime risks and tags
	var runtimeRisks []deploymentrecord.RuntimeRisk
	var tags map[string]string
	if aggPodMetadata != nil {
		for risk := range aggPodMetadata.RuntimeRisks {
			runtimeRisks = append(runtimeRisks, risk)
		}
		slices.Sort(runtimeRisks)
		tags = aggPodMetadata.Tags
	}

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
		tags,
	)

	if err := c.apiClient.PostOne(ctx, record); err != nil {
		// Return if no artifact is found and cache the digest
		var noArtifactErr *deploymentrecord.NoArtifactError
		if errors.As(err, &noArtifactErr) {
			c.unknownArtifacts.Set(digest, true, unknownArtifactTTL)
			slog.Info("No artifact found, digest cached as unknown",
				"deployment_name", dn,
				"digest", digest,
			)
			return nil
		}

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
		"digest", record.Digest,
		"runtime_risks", record.RuntimeRisks,
		"tags", record.Tags,
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
		// If there was a previous create event, remove that
		cacheKey = getCacheKey(EventCreated, dn, digest)
		c.observedDeployments.Delete(cacheKey)
	default:
		return fmt.Errorf("invalid status: %s", status)
	}

	return nil
}

func getCacheKey(ev, dn, digest string) string {
	return ev + "||" + dn + "||" + digest
}

// isNumeric returns true if s is non-empty and consists entirely of ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
// as the K8s deployment's name!)
// The deployment name must unique within logical, physical environment and
// the cluster.
func getARDeploymentName(p *corev1.Pod, c corev1.Container, tmpl, workloadName string) string {
	res := tmpl
	res = strings.ReplaceAll(res, TmplNS, p.Namespace)
	res = strings.ReplaceAll(res, TmplDN, workloadName)
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
			return ociutil.ExtractDigest(status.ImageID)
		}
	}

	// Check init container statuses
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == containerName {
			return ociutil.ExtractDigest(status.ImageID)
		}
	}

	return ""
}

// hasSupportedOwner returns true if the pod is owned by a supported
// workload controller (ReplicaSet for Deployments, DaemonSet, StatefulSet, or Job for Jobs/CronJobs).
func hasSupportedOwner(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" || owner.Kind == "DaemonSet" || owner.Kind == "StatefulSet" || owner.Kind == "Job" {
			return true
		}
	}
	return false
}

// isTerminalPhase returns true if the pod has reached a terminal phase
// (Succeeded or Failed). Used to catch short-lived Job pods that complete
// before the controller observes them in the Running phase.
func isTerminalPhase(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
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

// getJobOwnerName returns the Job name from the pod's owner references,
// if the pod is owned by a Job.
func getJobOwnerName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" {
			return owner.Name
		}
	}
	return ""
}

// getDaemonSetName returns the DaemonSet name for a pod, if it belongs
// to one. DaemonSet pods are owned directly by the DaemonSet.
func getDaemonSetName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return owner.Name
		}
	}
	return ""
}

// getStatefulSetName returns the StatefulSet name for a pod, if it belongs
// to one. StatefulSet pods are owned directly by the StatefulSet.
func getStatefulSetName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "StatefulSet" {
			return owner.Name
		}
	}
	return ""
}

// getWorkloadRef resolves the top-level workload that owns a pod.
// For Deployment-owned pods (via ReplicaSets), returns the Deployment name.
// For DaemonSet-owned pods, returns the DaemonSet name.
// For StatefulSet-owned pods, returns the StatefulSet name.
// For CronJob-owned pods (via Jobs), returns the CronJob name.
// For standalone Job-owned pods, returns the Job name.
func (c *Controller) getWorkloadRef(pod *corev1.Pod) workloadRef {
	// Check for Deployment (via ReplicaSet)
	if dn := getDeploymentName(pod); dn != "" {
		return workloadRef{Name: dn, Kind: "Deployment"}
	}

	// Check for DaemonSet (direct ownership)
	if dsn := getDaemonSetName(pod); dsn != "" {
		return workloadRef{Name: dsn, Kind: "DaemonSet"}
	}

	// Check for StatefulSet (direct ownership)
	if ssn := getStatefulSetName(pod); ssn != "" {
		return workloadRef{Name: ssn, Kind: "StatefulSet"}
	}

	// Check for Job
	jobName := getJobOwnerName(pod)
	if jobName == "" {
		return workloadRef{}
	}

	return c.resolveJobWorkload(pod.Namespace, jobName)
}

// resolveJobWorkload determines whether a Job is owned by a CronJob or is standalone.
func (c *Controller) resolveJobWorkload(namespace, jobName string) workloadRef {
	// Try to look up the Job to check for CronJob ownership
	if c.jobLister != nil {
		job, err := c.jobLister.Jobs(namespace).Get(jobName)
		if err == nil {
			for _, owner := range job.OwnerReferences {
				if owner.Kind == "CronJob" {
					return workloadRef{Name: owner.Name, Kind: "CronJob"}
				}
			}
			return workloadRef{Name: jobName, Kind: "Job"}
		}
	}

	// Job not found in cache - try CronJob name derivation as fallback.
	// CronJob-created Jobs follow the naming pattern: <cronjob-name>-<unix-timestamp>
	// where the suffix is always numeric. We validate the suffix is all digits to
	// reduce false matches from standalone Jobs that coincidentally share a prefix
	// with an existing CronJob. A residual false positive is still possible if a
	// standalone Job is named exactly <cronjob>-<digits>, but the primary path
	// (checking Job OwnerReferences) handles the common case; this fallback only
	// fires when the Job has already been garbage-collected.
	if c.cronJobLister != nil {
		lastDash := strings.LastIndex(jobName, "-")
		if lastDash > 0 {
			suffix := jobName[lastDash+1:]
			if isNumeric(suffix) {
				potentialCronJobName := jobName[:lastDash]
				if c.cronJobExists(namespace, potentialCronJobName) {
					return workloadRef{Name: potentialCronJobName, Kind: "CronJob"}
				}
			}
		}
	}

	// Standalone Job (possibly already deleted)
	return workloadRef{Name: jobName, Kind: "Job"}
}

// workloadActive checks if the parent workload for a pod still exists
// in the local informer cache.
func (c *Controller) workloadActive(namespace string, ref workloadRef) bool {
	switch ref.Kind {
	case "Deployment":
		return c.deploymentExists(namespace, ref.Name)
	case "DaemonSet":
		return c.daemonSetExists(namespace, ref.Name)
	case "StatefulSet":
		return c.statefulSetExists(namespace, ref.Name)
	case "CronJob":
		return c.cronJobExists(namespace, ref.Name)
	case "Job":
		return c.jobExists(namespace, ref.Name)
	default:
		return false
	}
}

// jobExists checks if a job exists in the local informer cache.
func (c *Controller) jobExists(namespace, name string) bool {
	_, err := c.jobLister.Jobs(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if job exists in cache, assuming it does",
			"namespace", namespace,
			"job", name,
			"error", err,
		)
		return true
	}
	return true
}

// daemonSetExists checks if a daemonset exists in the local informer cache.
func (c *Controller) daemonSetExists(namespace, name string) bool {
	_, err := c.daemonSetLister.DaemonSets(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if daemonset exists in cache, assuming it does",
			"namespace", namespace,
			"daemonset", name,
			"error", err,
		)
		return true
	}
	return true
}

// statefulSetExists checks if a statefulset exists in the local informer cache.
func (c *Controller) statefulSetExists(namespace, name string) bool {
	_, err := c.statefulSetLister.StatefulSets(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if statefulset exists in cache, assuming it does",
			"namespace", namespace,
			"statefulset", name,
			"error", err,
		)
		return true
	}
	return true
}

// cronJobExists checks if a cronjob exists in the local informer cache.
func (c *Controller) cronJobExists(namespace, name string) bool {
	_, err := c.cronJobLister.CronJobs(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if cronjob exists in cache, assuming it does",
			"namespace", namespace,
			"cronjob", name,
			"error", err,
		)
		return true
	}
	return true
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
