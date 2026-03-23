package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/github/deployment-tracker/internal/metadata"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	amcache "k8s.io/apimachinery/pkg/util/cache"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
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

// Controller is the Kubernetes controller for tracking deployments.
type Controller struct {
	clientset          kubernetes.Interface
	metadataAggregator podMetadataAggregator
	podInformer        cache.SharedIndexInformer
	deploymentInformer cache.SharedIndexInformer
	deploymentLister   appslisters.DeploymentLister
	workqueue          workqueue.TypedRateLimitingInterface[PodEvent]
	apiClient          deploymentRecordPoster
	cfg                *Config
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
	factory := createInformerFactory(clientset, namespace, excludeNamespaces)

	apiClient, err := newAPIClient(cfg)
	if err != nil {
		return nil, err
	}

	cntrl := &Controller{
		clientset:           clientset,
		metadataAggregator:  metadataAggregator,
		podInformer:         factory.Core().V1().Pods().Informer(),
		deploymentInformer:  factory.Apps().V1().Deployments().Informer(),
		deploymentLister:    factory.Apps().V1().Deployments().Lister(),
		workqueue:           workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[PodEvent]()),
		apiClient:           apiClient,
		cfg:                 cfg,
		observedDeployments: amcache.NewExpiring(),
		unknownArtifacts:    amcache.NewExpiring(),
	}

	if err := cntrl.registerEventHandlers(); err != nil {
		return nil, err
	}

	return cntrl, nil
}

// newAPIClient creates a deployment record API client from the controller config.
func newAPIClient(cfg *Config) (deploymentRecordPoster, error) {
	var clientOpts []deploymentrecord.ClientOption
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

	client, err := deploymentrecord.NewClient(
		cfg.BaseURL,
		cfg.Organization,
		clientOpts...,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create API client: %w", err)
	}
	return client, nil
}

// Run starts the controller.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	slog.Info("Starting informers")

	// Start the informers
	go c.podInformer.Run(ctx.Done())
	go c.deploymentInformer.Run(ctx.Done())

	// Wait for the caches to be synced
	slog.Info("Waiting for informer caches to sync")
	if !cache.WaitForCacheSync(ctx.Done(), c.podInformer.HasSynced, c.deploymentInformer.HasSynced) {
		return errors.New("timed out waiting for caches to sync")
	}

	slog.Info("Starting workers",
		"count", workers,
	)

	c.startWorkers(ctx, workers)

	slog.Info("Controller started")

	<-ctx.Done()
	slog.Info("Shutting down workers")

	return nil
}
