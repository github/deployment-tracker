package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/github/deployment-tracker/internal/metadata"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/github/deployment-tracker/pkg/dtmetrics"
	"github.com/github/deployment-tracker/pkg/ociutil"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

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
		if deploymentName != "" && c.deploymentExists(pod.Namespace, deploymentName) {
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
	var aggPodMetadata *metadata.AggregatePodMetadata
	if status != deploymentrecord.StatusDecommissioned {
		aggPodMetadata = c.metadataAggregator.BuildAggregatePodMetadata(ctx, podToPartialMetadata(pod))
	}

	// Record info for each container in the pod
	for _, container := range pod.Spec.Containers {
		if err := c.recordContainer(ctx, pod, container, status, event.EventType, aggPodMetadata); err != nil {
			lastErr = err
		}
	}

	// Also record init containers
	for _, container := range pod.Spec.InitContainers {
		if err := c.recordContainer(ctx, pod, container, status, event.EventType, aggPodMetadata); err != nil {
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
func (c *Controller) recordContainer(ctx context.Context, pod *corev1.Pod, container corev1.Container, status, eventType string, aggPodMetadata *metadata.AggregatePodMetadata) error {
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
