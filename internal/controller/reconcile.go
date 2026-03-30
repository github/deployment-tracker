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

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

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
