package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/github/deployment-tracker/internal/metadata"
	"github.com/github/deployment-tracker/internal/workload"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/github/deployment-tracker/pkg/dtmetrics"
	amcache "k8s.io/apimachinery/pkg/util/cache"

	corev1 "k8s.io/api/core/v1"
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

	// unknownArtifactTTL is the TTL for cached 404 responses from the
	// deployment record API. Once an artifact is known to be missing,
	// we suppress further API calls for this duration.
	unknownArtifactTTL = 1 * time.Hour

	// informerSyncTimeoutDuration is the maximum duration of time allowed
	// for the informers to sync to prevent the controller from hanging indefinitely.
	informerSyncTimeoutDuration = 60 * time.Second
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

// workloadResolver is an interface for resolving the workload identity of a pod
// and determining if a workload is active.
type workloadResolver interface {
	Resolve(pod *corev1.Pod) workload.Identity
	IsActive(namespace string, identity workload.Identity) bool
}

// PodEvent represents a pod event to be processed.
type PodEvent struct {
	Key        string
	EventType  string
	DeletedPod *corev1.Pod // Only populated for delete events
}

// Controller is the Kubernetes controller for tracking deployments.
type Controller struct {
	informerFactory    informers.SharedInformerFactory
	podInformer        cache.SharedIndexInformer
	workqueue          workqueue.TypedRateLimitingInterface[PodEvent]
	workloadResolver   workloadResolver
	metadataAggregator podMetadataAggregator
	apiClient          deploymentRecordPoster
	cfg                *Config
	// best effort cache to avoid redundant posts
	// post requests are idempotent, so if this cache fails due to
	// restarts or other events, nothing will break.
	observedDeployments ttlCache
	// best effort cache to suppress API calls for artifacts that
	// returned a 404 (no artifact found). Keyed by image digest.
	unknownArtifacts ttlCache
	// informerSyncTimeout is the maximum time allowed for all informers to sync
	// and prevents sync from hanging indefinitely.
	informerSyncTimeout time.Duration
}

// New creates a new deployment tracker controller.
func New(clientset kubernetes.Interface, metadataAggregator podMetadataAggregator, namespace string, excludeNamespaces string, cfg *Config) (*Controller, error) {
	// Create informer factory
	informerFactory := createInformerFactory(clientset, namespace, excludeNamespaces)

	podInformer := informerFactory.Core().V1().Pods().Informer()

	// Create the workload resolver with listers from the factory
	resolver := workload.NewResolver(
		informerFactory.Apps().V1().Deployments().Lister(),
		informerFactory.Apps().V1().DaemonSets().Lister(),
		informerFactory.Apps().V1().StatefulSets().Lister(),
		informerFactory.Batch().V1().Jobs().Lister(),
		informerFactory.Batch().V1().CronJobs().Lister(),
	)

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
		informerFactory:     informerFactory,
		podInformer:         podInformer,
		workqueue:           queue,
		workloadResolver:    resolver,
		metadataAggregator:  metadataAggregator,
		apiClient:           apiClient,
		cfg:                 cfg,
		observedDeployments: amcache.NewExpiring(),
		unknownArtifacts:    amcache.NewExpiring(),
		informerSyncTimeout: informerSyncTimeoutDuration,
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
			if pod.Status.Phase == corev1.PodRunning && workload.HasSupportedOwner(pod) {
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
			if workload.IsTerminalPhase(pod) && workload.GetJobOwnerName(pod) != "" {
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
			isJobTerminal := !workload.IsTerminalPhase(oldPod) && workload.IsTerminalPhase(newPod) &&
				oldPod.Status.Phase != corev1.PodRunning && workload.GetJobOwnerName(newPod) != ""
			if !isJobTerminal {
				if newPod.DeletionTimestamp != nil || !workload.HasSupportedOwner(newPod) {
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
			if !workload.HasSupportedOwner(pod) {
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

	// Start all informers via the factory
	c.informerFactory.Start(ctx.Done())

	// Wait for the caches to be synced
	slog.Info("Waiting for informer caches to sync")
	informerSyncCtx, cancel := context.WithTimeout(ctx, c.informerSyncTimeout)

	syncResults := c.informerFactory.WaitForCacheSync(informerSyncCtx.Done())
	cancel()

	for _, synced := range syncResults {
		if !synced {
			if ctx.Err() != nil {
				return fmt.Errorf("cache sync interrupted: %w", ctx.Err())
			}
			return errors.New("timed out waiting for caches to sync - please ensure deployment tracker has the correct kubernetes permissions")
		}
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
