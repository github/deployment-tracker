package controller

import (
	"context"
	"encoding/json"
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

func TestProcessSyncEvents_EmptyPodList(t *testing.T) {
	t.Parallel()
	poster := &mockPoster{}
	ctrl := newTestController(poster)

	err := ctrl.processSyncEvents(context.Background(), []any{})
	require.NoError(t, err)
	assert.Equal(t, 0, poster.getPostClusterCalls(), "PostCluster should not be called for empty pod list")
}

func TestProcessSyncEvents_HappyPath(t *testing.T) {
	t.Parallel()
	digest := "sha256:abc123"
	unknownDigest := "sha256:notfound999"
	unauthorizedDigest := "sha256:unauthorized999"
	clusterResp := deploymentrecord.RecordsClusterResp{
		TotalCount: 1,
		DeploymentRecords: []*deploymentrecord.RecordResp{{
			Record: deploymentrecord.Record{
				BaseRecord: deploymentrecord.BaseRecord{
					DeploymentName: "default/test-deploy/app",
					Digest:         digest,
				},
			},
		}},
		Errors: []*deploymentrecord.RecordErrorResp{
			{
				Record: deploymentrecord.Record{
					BaseRecord: deploymentrecord.BaseRecord{
						Digest: unknownDigest,
					},
				},
				Cause: "not_found",
			},
			{
				Record: deploymentrecord.Record{
					BaseRecord: deploymentrecord.BaseRecord{
						Digest: unauthorizedDigest,
					},
				},
				Cause: "unauthorized",
			},
		},
	}
	respJSON, err := json.Marshal(clusterResp)
	require.NoError(t, err)

	poster := &mockPoster{clusterResp: respJSON}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}

	err = ctrl.processSyncEvents(context.Background(), []any{
		makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet"),
		makeTestPod("unknown", "test-deploy-abc456", unknownDigest, "ReplicaSet"),
		makeTestPod("unauthorized", "test-deploy-abc789", unauthorizedDigest, "Job"),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostClusterCalls(), "PostCluster should be called once")
	assert.Equal(t, 3, poster.clusterRecordCount, "PostCluster should receive 3 records")

	// Successful record should be in observedDeployments cache
	cacheKey := getCacheKey(EventCreated, "default/test-deploy/app", digest)
	_, exists := ctrl.observedDeployments.Get(cacheKey)
	assert.True(t, exists, "successful record should populate observedDeployments cache")

	// not_found error should be in unknownArtifacts cache
	_, exists = ctrl.unknownArtifacts.Get("sha256:notfound999")
	assert.True(t, exists, "not_found error should populate unknownArtifacts cache")

	// unauthorized error should not be in unknownArtifacts cache
	_, exists = ctrl.unknownArtifacts.Get("sha256:unauthorized999")
	assert.False(t, exists, "unauthorized error should not populate unknownArtifacts cache")
}

func TestProcessSyncEvents_DedupeContainers(t *testing.T) {
	t.Parallel()
	digest := "sha256:abc123"
	poster := &mockPoster{}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}

	pod := makeTestPod("app", "test-deploy-abc123", digest, "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod, pod})
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getPostClusterCalls(), "PostCluster should be called once")
	assert.Equal(t, 1, poster.clusterRecordCount, "PostCluster should receive only 1 record")
}

func TestProcessSyncEvents_PostCluster404(t *testing.T) {
	t.Parallel()
	poster := &mockPoster{
		clusterErr: &deploymentrecord.ClusterNoRepositoriesError{},
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}
	pod := makeTestPod("app", "test-deploy-abc123", "sha256:abc123", "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod})
	require.NoError(t, err, "ClusterNoRepositoriesError should not propagate")
	assert.Equal(t, 1, poster.getPostClusterCalls())

	// Caches should remain empty since no response was processed
	cacheKey := getCacheKey(EventCreated, "default/test-deploy/app", "sha256:abc123")
	_, exists := ctrl.observedDeployments.Get(cacheKey)
	assert.False(t, exists, "observedDeployments should not be populated on 404")
}

func TestProcessSyncEvents_PostCluster500(t *testing.T) {
	t.Parallel()
	poster := &mockPoster{
		clusterErr: errors.New("server error"),
	}
	ctrl := newTestController(poster)
	ctrl.workloadResolver = &mockResolver{name: "test-deploy"}
	pod := makeTestPod("app", "test-deploy-abc123", "sha256:abc123", "ReplicaSet")

	err := ctrl.processSyncEvents(context.Background(), []any{pod})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to post sync cluster records")
	assert.Equal(t, 1, poster.getPostClusterCalls())
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
