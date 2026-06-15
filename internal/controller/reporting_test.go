package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRecordContainer_UnknownArtifactCachePopulatedOn404(t *testing.T) {
	t.Parallel()
	digest := "sha256:unknown404digest"
	poster := &mockPoster{
		lastErr: &deploymentrecord.NoArtifactError{},
	}
	ctrl := newTestController(poster)
	pod, container := testPod(digest)

	// First call should hit the API and get a 404
	err := ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostOneCalls())

	// Digest should now be in the unknown artifacts cache
	_, exists := ctrl.unknownArtifacts.Get(digest)
	assert.True(t, exists, "digest should be cached after 404")
}

func TestRecordContainer_UnknownArtifactCacheSkipsAPICall(t *testing.T) {
	t.Parallel()
	digest := "sha256:cacheddigest"
	poster := &mockPoster{
		lastErr: &deploymentrecord.NoArtifactError{},
	}
	ctrl := newTestController(poster)
	pod, container := testPod(digest)

	// First call — API returns 404, populates cache
	err := ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostOneCalls())

	// Second call — should be served from cache, no API call
	err = ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostOneCalls(), "API should not be called for cached unknown artifact")
}

func TestRecordContainer_UnknownArtifactCacheAppliesToDecommission(t *testing.T) {
	t.Parallel()
	digest := "sha256:decommission404"
	poster := &mockPoster{
		lastErr: &deploymentrecord.NoArtifactError{},
	}
	ctrl := newTestController(poster)
	pod, container := testPod(digest)

	// Deploy call — 404, populates cache
	err := ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostOneCalls())

	// Decommission call for same digest — should skip API
	err = ctrl.recordContainer(context.Background(), pod, container, EventDeleted, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostOneCalls(), "decommission should also be skipped for cached unknown artifact")
}

func TestRecordContainer_UnknownArtifactCacheExpires(t *testing.T) {
	t.Parallel()
	digest := "sha256:expiringdigest"
	poster := &mockPoster{
		lastErr: &deploymentrecord.NoArtifactError{},
	}
	ctrl := newTestController(poster)
	pod, container := testPod(digest)

	// Seed the cache with a very short TTL to test expiry
	ctrl.unknownArtifacts.Set(digest, true, 50*time.Millisecond)

	// Immediately — should be cached
	err := ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 0, poster.getPostOneCalls(), "should skip API while cached")

	// Wait for expiry
	time.Sleep(100 * time.Millisecond)

	// After expiry — should call API again
	err = ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostOneCalls(), "should call API after cache expiry")
}

func TestRecordContainer_SuccessfulPostDoesNotPopulateUnknownCache(t *testing.T) {
	t.Parallel()
	digest := "sha256:knowndigest"
	poster := &mockPoster{lastErr: nil} // success
	ctrl := newTestController(poster)
	pod, container := testPod(digest)

	err := ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostOneCalls())

	// Digest should NOT be in the unknown artifacts cache
	_, exists := ctrl.unknownArtifacts.Get(digest)
	assert.False(t, exists, "successful post should not cache digest as unknown")
}

func TestProcessSyncEvents_DisabledSkipsSync(t *testing.T) {
	t.Parallel()
	poster := &mockPoster{
		jobResp:   &deploymentrecord.JobResponse{JobID: 1},
		jobStatus: &deploymentrecord.JobStatus{Status: "completed"},
	}
	ctrl := newTestController(poster)
	ctrl.cfg.BulkClusterSync = false
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}
	pod := makeTestPod("app", "test-deploy-abc123", "sha256:abc123", "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod})
	require.NoError(t, err)
	assert.Equal(t, 0, poster.getCreateClusterJobCalls(), "should not call CreateClusterJob when disabled")
}

func TestProcessSyncEvents_EmptyPodList(t *testing.T) {
	t.Parallel()
	poster := &mockPoster{}
	ctrl := newTestController(poster)

	err := ctrl.processSyncEvents(context.Background(), []any{})
	require.NoError(t, err)
	assert.Equal(t, 0, poster.getCreateClusterJobCalls(), "CreateClusterJob should not be called for empty pod list")
}

func TestProcessSyncEvents_HappyPath(t *testing.T) {
	t.Parallel()
	digest := "sha256:abc123"
	unknownDigest := "sha256:notfound999"

	// Use distinct image names so name→digest mapping is unambiguous.
	knownPod := makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet")
	unknownPod := makeTestPod("sidecar", "test-deploy-abc456", unknownDigest, "ReplicaSet")
	unknownPod.Spec.Containers[0].Image = "busybox:latest"

	poster := &mockPoster{
		jobResp: &deploymentrecord.JobResponse{
			JobID: 42,
			// Name is the image name, matched against submitted records
			// to resolve the digest for unknownArtifacts cache keying.
			Errors: []deploymentrecord.JobError{
				{Name: "busybox", Cause: "not_found"},
			},
		},
		jobStatus: &deploymentrecord.JobStatus{
			JobID:      42,
			Status:     "completed",
			TotalCount: 2,
		},
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}

	err := ctrl.processSyncEvents(context.Background(), []any{knownPod, unknownPod})
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getCreateClusterJobCalls(), "CreateClusterJob should be called once")
	assert.Equal(t, 1, poster.getWaitForClusterJobCalls(), "WaitForClusterJob should be called once")
	assert.Equal(t, 2, poster.clusterRecordCount, "CreateClusterJob should receive 2 records")

	// All submitted records should be in observedDeployments cache
	cacheKey := getCacheKey(EventCreated, "default/test-deploy/app", digest)
	_, exists := ctrl.observedDeployments.Get(cacheKey)
	assert.True(t, exists, "submitted record should populate observedDeployments cache")

	// not_found error for "busybox" should cache its digest (unambiguous mapping)
	_, exists = ctrl.unknownArtifacts.Get(unknownDigest)
	assert.True(t, exists, "not_found error should populate unknownArtifacts cache by digest")

	// Known digest should NOT be in unknownArtifacts
	_, exists = ctrl.unknownArtifacts.Get(digest)
	assert.False(t, exists, "known artifact should not be in unknownArtifacts cache")
}

func TestProcessSyncEvents_AmbiguousImageNameSkipsUnknownCache(t *testing.T) {
	t.Parallel()
	digest1 := "sha256:abc123"
	digest2 := "sha256:def456"

	// Both pods use the same image name "nginx" with different digests.
	// The name→digest mapping is ambiguous, so not_found errors should
	// NOT populate unknownArtifacts (we can't tell which digest to cache).
	poster := &mockPoster{
		jobResp: &deploymentrecord.JobResponse{
			JobID: 42,
			Errors: []deploymentrecord.JobError{
				{Name: "nginx", Cause: "not_found"},
			},
		},
		jobStatus: &deploymentrecord.JobStatus{
			JobID:      42,
			Status:     "completed",
			TotalCount: 2,
		},
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}

	err := ctrl.processSyncEvents(context.Background(), []any{
		makeTestPod("app", "test-deploy-abc123", digest1, "ReplicaSet"),
		makeTestPod("sidecar", "test-deploy-abc456", digest2, "ReplicaSet"),
	})
	require.NoError(t, err)

	// Neither digest should be cached since "nginx" maps to two digests
	_, exists := ctrl.unknownArtifacts.Get(digest1)
	assert.False(t, exists, "ambiguous name should not cache first digest")
	_, exists = ctrl.unknownArtifacts.Get(digest2)
	assert.False(t, exists, "ambiguous name should not cache second digest")

	// But observedDeployments should still be populated for both
	cacheKey1 := getCacheKey(EventCreated, "default/test-deploy/app", digest1)
	_, exists = ctrl.observedDeployments.Get(cacheKey1)
	assert.True(t, exists, "observedDeployments should be populated regardless")
}

func TestProcessSyncEvents_DedupeContainers(t *testing.T) {
	t.Parallel()
	digest := "sha256:abc123"
	poster := &mockPoster{
		jobResp: &deploymentrecord.JobResponse{JobID: 1},
		jobStatus: &deploymentrecord.JobStatus{
			JobID:  1,
			Status: "completed",
		},
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}

	pod := makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod, pod})
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getCreateClusterJobCalls(), "CreateClusterJob should be called once")
	assert.Equal(t, 1, poster.clusterRecordCount, "CreateClusterJob should receive only 1 record")
}

func TestProcessSyncEvents_409Conflict(t *testing.T) {
	t.Parallel()
	digest := "sha256:abc123"
	poster := &mockPoster{
		jobErr: &deploymentrecord.ClusterJobConflictError{},
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}
	pod := makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod})
	require.NoError(t, err, "409 conflict should not propagate as error")
	assert.Equal(t, 1, poster.getCreateClusterJobCalls())
	assert.Equal(t, 0, poster.getWaitForClusterJobCalls(), "should not wait on conflict")

	// Caches should be populated from submitted records
	cacheKey := getCacheKey(EventCreated, "default/test-deploy/app", digest)
	_, exists := ctrl.observedDeployments.Get(cacheKey)
	assert.True(t, exists, "observedDeployments should be populated from submitted records on 409")
}

func TestProcessSyncEvents_AsyncEndpointNotAvailable(t *testing.T) {
	t.Parallel()
	digest := "sha256:abc123"
	poster := &mockPoster{
		jobErr: &deploymentrecord.ClusterNoRepositoriesError{},
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}
	pod := makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod})
	require.NoError(t, err, "404 should not propagate as error")
	assert.Equal(t, 1, poster.getCreateClusterJobCalls())
	assert.Equal(t, 0, poster.getWaitForClusterJobCalls(), "should not wait on 404")

	// Caches should remain empty — no data to fill from
	cacheKey := getCacheKey(EventCreated, "default/test-deploy/app", digest)
	_, exists := ctrl.observedDeployments.Get(cacheKey)
	assert.False(t, exists, "observedDeployments should not be populated on 404")
}

func TestProcessSyncEvents_JobCreationFailed(t *testing.T) {
	t.Parallel()
	poster := &mockPoster{
		jobErr: errors.New("server error"),
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}
	pod := makeTestPod("app", "test-deploy-abc123", "sha256:abc123", "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create cluster job")
	assert.Equal(t, 1, poster.getCreateClusterJobCalls())
}

func TestProcessSyncEvents_JobWaitFailed(t *testing.T) {
	t.Parallel()
	digest := "sha256:abc123"
	poster := &mockPoster{
		jobResp:    &deploymentrecord.JobResponse{JobID: 42},
		jobWaitErr: errors.New("context deadline exceeded"),
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}
	pod := makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod})
	require.NoError(t, err, "job wait failure should not block startup")
	assert.Equal(t, 1, poster.getCreateClusterJobCalls())
	assert.Equal(t, 1, poster.getWaitForClusterJobCalls())

	// Should still fill caches from submitted records
	cacheKey := getCacheKey(EventCreated, "default/test-deploy/app", digest)
	_, exists := ctrl.observedDeployments.Get(cacheKey)
	assert.True(t, exists, "observedDeployments should be populated from submitted records on wait failure")
}

func TestMakeSyncRecords_TerminalJobPodIncluded(t *testing.T) {
	t.Parallel()
	digest := "sha256:terminal-job-digest"
	poster := &mockPoster{}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-job"}

	pod := makeTestPod("worker", "test-job-abc123", digest, "Job")
	pod.Status.Phase = corev1.PodSucceeded

	records := ctrl.makeSyncRecords(context.Background(), []any{pod})
	assert.Len(t, records, 1, "terminal Job pod should be included in sync records")
}

func TestMakeSyncRecords_FailedJobPodIncluded(t *testing.T) {
	t.Parallel()
	digest := "sha256:failed-job-digest"
	poster := &mockPoster{}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-job"}

	pod := makeTestPod("worker", "test-job-abc123", digest, "Job")
	pod.Status.Phase = corev1.PodFailed

	records := ctrl.makeSyncRecords(context.Background(), []any{pod})
	assert.Len(t, records, 1, "failed Job pod should be included in sync records")
}

func TestMakeSyncRecords_TerminalNonJobPodExcluded(t *testing.T) {
	t.Parallel()
	digest := "sha256:terminal-non-job-digest"
	poster := &mockPoster{}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}

	pod := makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet")
	pod.Status.Phase = corev1.PodSucceeded

	records := ctrl.makeSyncRecords(context.Background(), []any{pod})
	assert.Empty(t, records, "terminal non-Job pod should not be included in sync records")
}

func TestMakeSyncRecords_PendingJobPodExcluded(t *testing.T) {
	t.Parallel()
	digest := "sha256:pending-job-digest"
	poster := &mockPoster{}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-job"}

	pod := makeTestPod("worker", "test-job-abc123", digest, "Job")
	pod.Status.Phase = corev1.PodPending

	records := ctrl.makeSyncRecords(context.Background(), []any{pod})
	assert.Empty(t, records, "pending Job pod should not be included in sync records")
}

func makeTestPod(containerName string, parentName string, digest string, parentKind string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: parentKind,
				Name: parentName,
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  containerName,
				Image: "nginx:latest",
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    containerName,
				ImageID: fmt.Sprintf("docker-pullable://nginx@%s", digest),
			}},
		},
	}
}
