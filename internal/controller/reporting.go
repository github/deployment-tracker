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

	// Gather aggregate metadata for adds/updates
	var aggPodMetadata *metadata.AggregatePodMetadata
	if event.EventType != EventDeleted {
		aggPodMetadata = c.metadataAggregator.BuildAggregatePodMetadata(ctx, podToPartialMetadata(pod))
	}

	var lastErr error

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

func (c *Controller) processSyncEvents(ctx context.Context, syncClusterPods []any) error {
	if !c.cfg.BulkClusterSync {
		slog.Info("Async cluster sync disabled, skipping startup sync")
		return nil
	}

	syncRecords := c.makeSyncRecords(ctx, syncClusterPods)
	if len(syncRecords) == 0 {
		slog.Info("No sync records to post")
		return nil
	}

	jobResp, err := c.apiClient.CreateClusterJob(ctx, syncRecords, c.cfg.Cluster)
	if err != nil {
		var conflictErr *deploymentrecord.ClusterJobConflictError
		var noReposErr *deploymentrecord.ClusterNoRepositoriesError

		switch {
		case errors.As(err, &conflictErr):
			slog.Warn("Cluster job already in progress, skipping startup sync",
				"org", c.cfg.Organization,
			)
			c.fillCachesFromSubmitted(syncRecords)
			return nil

		case errors.As(err, &noReposErr):
			slog.Info("Async cluster endpoint not available, skipping startup sync",
				"org", c.cfg.Organization,
			)
			return nil

		default:
			slog.Error("Failed to create cluster job",
				"error", err,
				"record_count", len(syncRecords),
			)
			return fmt.Errorf("failed to create cluster job: %w", err)
		}
	}

	if len(jobResp.Errors) > 0 {
		slog.Warn("Some deployments rejected from job submission",
			"job_id", jobResp.JobID,
			"rejected_count", len(jobResp.Errors),
		)
	}

	// Log per-deployment at INFO now that the job is queued, matching the
	// individual "Posted record" path so startup sync has equivalent
	// per-deployment visibility. Records rejected at submission are excluded.
	logSyncedRecords(jobResp, syncRecords)

	// Wait for job completion with a timeout to prevent indefinite startup delay.
	jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	jobStatus, err := c.apiClient.WaitForClusterJob(jobCtx, c.cfg.Cluster, jobResp.JobID)
	if err != nil {
		slog.Error("Failed waiting for cluster job, filling caches from submitted records",
			"job_id", jobResp.JobID,
			"error", err,
		)
		c.fillCachesFromSubmitted(syncRecords)
		return nil
	}

	slog.Info("Cluster job completed",
		"job_id", jobResp.JobID,
		"status", jobStatus.Status,
		"total_count", jobStatus.TotalCount,
		"errors", len(jobStatus.Errors),
	)

	// If the job failed, don't populate observedDeployments — records may not
	// have been created, and suppressing re-posts would delay self-healing.
	// Only populate unknownArtifacts from errors so we avoid redundant 404s.
	if jobStatus.Status == "failed" {
		slog.Warn("Cluster job failed, skipping observedDeployments cache fill",
			"job_id", jobResp.JobID,
		)
		return nil
	}

	c.fillCachesFromJobResult(syncRecords, jobResp, jobStatus)
	return nil
}

// syncCandidate is a single container observed on a pod that is eligible to
// become the deployment record for a given deployment_name during startup sync.
type syncCandidate struct {
	pod       *corev1.Pod
	container corev1.Container
	wlName    string
}

func (c *Controller) makeSyncRecords(ctx context.Context, syncClusterPods []any) []*deploymentrecord.Record {
	// The API requires a unique set of deployment_names for a snapshot job.
	// During a rollout the same deployment_name can appear with multiple digests
	// (old and new pods running at once), so we must collapse to a single digest per workload +
	// container name. We pick the newest running pod so the snapshot reflects the rollout target.
	winners := make(map[string]syncCandidate)
	for _, p := range syncClusterPods {
		pod, ok := p.(*corev1.Pod)
		if !ok {
			slog.Error("Invalid object type in sync cluster pod list")
			continue
		}

		isRunningSupported := pod.Status.Phase == corev1.PodRunning && workload.HasSupportedOwner(pod)
		isTerminalJob := workload.IsTerminalPhase(pod) && workload.GetJobOwnerName(pod) != ""
		if !isRunningSupported && !isTerminalJob {
			continue
		}

		// Resolve the workload name for the deployment record.
		wl := c.workloadResolver.Resolve(pod)
		if wl.Name == "" {
			slog.Debug("Could not resolve workload name for sync pod, skipping",
				"namespace", pod.Namespace,
				"pod", pod.Name,
			)
			continue
		}

		allContainers := make([]corev1.Container, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
		allContainers = append(allContainers, pod.Spec.Containers...)
		allContainers = append(allContainers, pod.Spec.InitContainers...)

		// Dedupe to one record per deployment_name. On a digest conflict
		// (rollout in progress) the newest running pod wins.
		for _, container := range allContainers {
			dn := getARDeploymentName(pod, container, c.cfg.Template, wl.Name)
			digest := getContainerDigest(pod, container.Name)
			if dn == "" || digest == "" {
				continue
			}

			existing, exists := winners[dn]
			if !exists {
				winners[dn] = syncCandidate{pod: pod, container: container, wlName: wl.Name}
				continue
			}

			// Same deployment_name with the same digest is just another replica
			// of the same version. The submitted record (deployment_name, digest,
			// image) is identical regardless of which pod we pick, so keep the
			// one we already have.
			existingDigest := getContainerDigest(existing.pod, existing.container.Name)
			if existingDigest == digest {
				continue
			}

			// Collision with a different digest (e.g. rollout in progress).
			// Keep the newest running pod's digest and log the decision so the
			// rollout is visible.
			if isPreferredSyncPod(pod, existing.pod) {
				slog.Info("Multiple digests observed for deployment_name during sync, choosing newest running",
					"deployment_name", dn,
					"chosen_pod", pod.Name,
					"chosen_digest", digest,
					"replaced_pod", existing.pod.Name,
					"replaced_digest", existingDigest,
				)
				winners[dn] = syncCandidate{pod: pod, container: container, wlName: wl.Name}
			} else {
				slog.Info("Multiple digests observed for deployment_name during sync, keeping newest running",
					"deployment_name", dn,
					"kept_pod", existing.pod.Name,
					"kept_digest", existingDigest,
					"skipped_pod", pod.Name,
					"skipped_digest", digest,
				)
			}
		}
	}

	// Build records from the winning candidates. Iterate in sorted
	// deployment_name order for deterministic output, and cache aggregate
	// metadata per pod so pods hosting multiple winners aren't recomputed.
	aggCache := make(map[*corev1.Pod]*metadata.AggregatePodMetadata)
	dns := make([]string, 0, len(winners))
	for dn := range winners {
		dns = append(dns, dn)
	}
	slices.Sort(dns)

	syncRecords := make([]*deploymentrecord.Record, 0, len(winners))
	for _, dn := range dns {
		cand := winners[dn]
		agg, ok := aggCache[cand.pod]
		if !ok {
			agg = c.metadataAggregator.BuildAggregatePodMetadata(ctx, podToPartialMetadata(cand.pod))
			aggCache[cand.pod] = agg
		}
		record := c.buildRecord(cand.pod, cand.container, EventCreated, cand.wlName, agg)
		if record != nil {
			syncRecords = append(syncRecords, record)
		}
	}

	slog.Info("Created sync records",
		"count", len(syncRecords),
	)
	return syncRecords
}

// isPreferredSyncPod reports whether candidate should replace current as the
// chosen pod for a deployment_name during startup sync. Running pods are
// preferred over terminal pods; among pods in the same category the most
// recently created pod wins. This makes the startup snapshot reflect the
// rollout target (newest running digest) when multiple digests coexist.
func isPreferredSyncPod(candidate, current *corev1.Pod) bool {
	candRunning := candidate.Status.Phase == corev1.PodRunning
	curRunning := current.Status.Phase == corev1.PodRunning
	if candRunning != curRunning {
		return candRunning
	}
	return candidate.CreationTimestamp.After(current.CreationTimestamp.Time)
}

// logSyncedRecords emits one log line per accepted record after a cluster job
// has been queued.
func logSyncedRecords(jobResp *deploymentrecord.JobResponse, records []*deploymentrecord.Record) {
	rejected := make(map[string]bool, len(jobResp.Errors))
	for _, e := range jobResp.Errors {
		rejected[e.Name] = true
	}

	for _, r := range records {
		if rejected[r.Name] {
			continue
		}
		slog.Info("Submitted record for sync",
			"event_type", EventCreated,
			"name", r.Name,
			"deployment_name", r.DeploymentName,
			"status", r.Status,
			"digest", r.Digest,
			"runtime_risks", r.RuntimeRisks,
			"tags", r.Tags,
			"job_id", jobResp.JobID,
		)
	}
}

// fillCachesFromSubmitted populates the observedDeployments cache from the
// records we submitted, without waiting for a response. Used when we can't
// get a response (409 conflict, wait timeout, job failure).
func (c *Controller) fillCachesFromSubmitted(records []*deploymentrecord.Record) {
	slog.Info("Filling observedDeployments cache from submitted records",
		"count", len(records),
	)
	for _, r := range records {
		cacheKey := getCacheKey(EventCreated, r.DeploymentName, r.Digest)
		c.observedDeployments.Set(cacheKey, true, 2*time.Minute)
	}
}

// fillCachesFromJobResult populates both caches after an async job completes.
// observedDeployments is filled from submitted records, and unknownArtifacts
// is filled from error responses with cause "not_found".
func (c *Controller) fillCachesFromJobResult(records []*deploymentrecord.Record, jobResp *deploymentrecord.JobResponse, jobStatus *deploymentrecord.JobStatus) {
	slog.Info("Filling caches after cluster job completion",
		"record_count", len(records),
	)

	// Build a name→digests lookup from submitted records so we can
	// key unknownArtifacts by digest (which is how recordContainer looks them up).
	// Multiple records can share the same image name with different digests,
	// so we only cache when the mapping is unambiguous (exactly one digest per name).
	nameToDigests := make(map[string][]string, len(records))
	for _, r := range records {
		cacheKey := getCacheKey(EventCreated, r.DeploymentName, r.Digest)
		c.observedDeployments.Set(cacheKey, true, 2*time.Minute)
		nameToDigests[r.Name] = append(nameToDigests[r.Name], r.Digest)
	}

	cacheUnknownDigests := func(jobErrors []deploymentrecord.JobError) {
		for _, e := range jobErrors {
			if e.Cause == "not_found" {
				if digests, ok := nameToDigests[e.Name]; ok && len(digests) == 1 {
					c.unknownArtifacts.Set(digests[0], true, unknownArtifactTTL)
				}
			}
		}
	}

	// Fill unknownArtifacts from job submission and completion errors
	cacheUnknownDigests(jobResp.Errors)
	cacheUnknownDigests(jobStatus.Errors)
}

// recordContainer records a single container's deployment info.
func (c *Controller) recordContainer(ctx context.Context, pod *corev1.Pod, container corev1.Container, eventType, workloadName string, aggPodMetadata *metadata.AggregatePodMetadata) error {
	// Create deployment record
	record := c.buildRecord(pod, container, eventType, workloadName, aggPodMetadata)
	if record == nil {
		slog.Debug("Unable to build record for container, skipping",
			"deployment_name", getARDeploymentName(pod, container, c.cfg.Template, workloadName))
		return nil
	}

	// Check if we've already recorded this deployment
	var cacheKey string
	switch record.Status {
	case deploymentrecord.StatusDeployed:
		cacheKey = getCacheKey(EventCreated, record.DeploymentName, record.Digest)
		if _, exists := c.observedDeployments.Get(cacheKey); exists {
			slog.Debug("Deployment already observed, skipping post",
				"deployment_name", record.DeploymentName,
				"digest", record.Digest,
			)
			return nil
		}
	case deploymentrecord.StatusDecommissioned:
		cacheKey = getCacheKey(EventDeleted, record.DeploymentName, record.Digest)
		if _, exists := c.observedDeployments.Get(cacheKey); exists {
			slog.Debug("Deployment already deleted, skipping post",
				"deployment_name", record.DeploymentName,
				"digest", record.Digest,
			)
			return nil
		}
	default:
		return fmt.Errorf("invalid status: %s", record.Status)
	}

	// Check if this artifact was previously unknown (404 from the API)
	if _, exists := c.unknownArtifacts.Get(record.Digest); exists {
		dtmetrics.PostDeploymentRecordUnknownArtifactCacheHit.Inc()
		slog.Debug("Artifact previously returned 404, skipping post",
			"deployment_name", record.DeploymentName,
			"digest", record.Digest,
		)
		return nil
	}

	if err := c.apiClient.PostOne(ctx, record); err != nil {
		// Return if no artifact is found and cache the digest
		var noArtifactErr *deploymentrecord.NoArtifactError
		if errors.As(err, &noArtifactErr) {
			c.unknownArtifacts.Set(record.Digest, true, unknownArtifactTTL)
			slog.Info("No artifact found, digest cached as unknown",
				"deployment_name", record.DeploymentName,
				"digest", record.Digest,
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
	switch record.Status {
	case deploymentrecord.StatusDeployed:
		cacheKey = getCacheKey(EventCreated, record.DeploymentName, record.Digest)
		c.observedDeployments.Set(cacheKey, true, 2*time.Minute)
		// If there was a previous delete event, remove that
		cacheKey = getCacheKey(EventDeleted, record.DeploymentName, record.Digest)
		c.observedDeployments.Delete(cacheKey)
	case deploymentrecord.StatusDecommissioned:
		cacheKey = getCacheKey(EventDeleted, record.DeploymentName, record.Digest)
		c.observedDeployments.Set(cacheKey, true, 2*time.Minute)
		// If there was a previous create event, remove that
		cacheKey = getCacheKey(EventCreated, record.DeploymentName, record.Digest)
		c.observedDeployments.Delete(cacheKey)
	default:
		return fmt.Errorf("invalid status: %s", record.Status)
	}

	return nil
}

func (c *Controller) buildRecord(pod *corev1.Pod, container corev1.Container, eventType, workloadName string, aggPodMetadata *metadata.AggregatePodMetadata) *deploymentrecord.Record {
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

	status := deploymentrecord.StatusDeployed
	if eventType == EventDeleted {
		status = deploymentrecord.StatusDecommissioned
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
	return deploymentrecord.NewDeploymentRecord(
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
