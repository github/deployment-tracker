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
	"github.com/github/deployment-tracker/internal/workload"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/github/deployment-tracker/pkg/dtmetrics"
	"github.com/github/deployment-tracker/pkg/ociutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// processEvent processes a single pod event.
func (c *Controller) processEvent(ctx context.Context, event PodEvent) error {
	var pod *corev1.Pod
	var wl workload.Identity

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
		wl = c.workloadResolver.Resolve(pod)
		if wl.Name != "" && c.workloadResolver.IsActive(pod.Namespace, wl) {
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
		wl = c.workloadResolver.Resolve(pod)
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

func podToPartialMetadata(pod *corev1.Pod) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: pod.ObjectMeta,
	}
}
