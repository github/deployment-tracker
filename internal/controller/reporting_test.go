package controller

import (
	"context"
	"testing"
	"time"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, 1, poster.getCalls())

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
	assert.Equal(t, 1, poster.getCalls())

	// Second call — should be served from cache, no API call
	err = ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getCalls(), "API should not be called for cached unknown artifact")
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
	assert.Equal(t, 1, poster.getCalls())

	// Decommission call for same digest — should skip API
	err = ctrl.recordContainer(context.Background(), pod, container, EventDeleted, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getCalls(), "decommission should also be skipped for cached unknown artifact")
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
	assert.Equal(t, 0, poster.getCalls(), "should skip API while cached")

	// Wait for expiry
	time.Sleep(100 * time.Millisecond)

	// After expiry — should call API again
	err = ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getCalls(), "should call API after cache expiry")
}

func TestRecordContainer_SuccessfulPostDoesNotPopulateUnknownCache(t *testing.T) {
	t.Parallel()
	digest := "sha256:knowndigest"
	poster := &mockPoster{lastErr: nil} // success
	ctrl := newTestController(poster)
	pod, container := testPod(digest)

	err := ctrl.recordContainer(context.Background(), pod, container, EventCreated, "test-deployment", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, poster.getCalls())

	// Digest should NOT be in the unknown artifacts cache
	_, exists := ctrl.unknownArtifacts.Get(digest)
	assert.False(t, exists, "successful post should not cache digest as unknown")
}
