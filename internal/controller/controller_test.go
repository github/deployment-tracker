package controller

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	amcache "k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
)

// mockPoster records all PostOne calls and returns a configurable error.
type mockPoster struct {
	mu      sync.Mutex
	calls   int
	lastErr error
}

func (m *mockPoster) PostOne(_ context.Context, _ *deploymentrecord.DeploymentRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.lastErr
}

func (m *mockPoster) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// newTestController creates a minimal Controller suitable for unit-testing
// recordContainer without a real Kubernetes cluster.
func newTestController(poster *mockPoster) *Controller {
	return &Controller{
		apiClient: poster,
		cfg: &Config{
			Template:            "{{namespace}}/{{deploymentName}}/{{containerName}}",
			LogicalEnvironment:  "test",
			PhysicalEnvironment: "test",
			Cluster:             "test",
		},
		observedDeployments: amcache.NewExpiring(),
		unknownArtifacts:    amcache.NewExpiring(),
	}
}

// testPod returns a pod with a single container and a known digest.
func testPod(digest string) (*corev1.Pod, corev1.Container) {
	container := corev1.Container{
		Name:  "app",
		Image: "nginx:latest",
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       "test-deployment-abc123",
			}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:    "app",
				ImageID: fmt.Sprintf("docker-pullable://nginx@%s", digest),
			}},
		},
	}
	return pod, container
}

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

func TestHasSupportedOwner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "pod owned by ReplicaSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "ReplicaSet",
						Name: "test-rs-abc123",
					}},
				},
			},
			expected: true,
		},
		{
			name: "pod owned by Job",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "Job",
						Name: "test-job",
					}},
				},
			},
			expected: true,
		},
		{
			name: "pod with no owner",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expected: false,
		},
		{
			name: "pod owned by DaemonSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "DaemonSet",
						Name: "test-ds",
					}},
				},
			},
			expected: true,
		},
		{
			name: "pod owned by StatefulSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "StatefulSet",
						Name: "test-ss",
					}},
				},
			},
			expected: true,
		},
		{
			name: "pod owned by ReplicationController",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "ReplicationController",
						Name: "test-rc",
					}},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := hasSupportedOwner(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetJobOwnerName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected string
	}{
		{
			name: "pod owned by Job",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "Job",
						Name: "my-job",
					}},
				},
			},
			expected: "my-job",
		},
		{
			name: "pod not owned by Job",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "ReplicaSet",
						Name: "my-rs-abc123",
					}},
				},
			},
			expected: "",
		},
		{
			name: "pod with no owner",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := getJobOwnerName(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetWorkloadRef_Deployment(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(&mockPoster{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet",
				Name: "my-deployment-abc123",
			}},
		},
	}
	wl := ctrl.getWorkloadRef(pod)
	assert.Equal(t, "my-deployment", wl.Name)
	assert.Equal(t, "Deployment", wl.Kind)
}

func TestGetWorkloadRef_DaemonSet(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(&mockPoster{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "DaemonSet",
				Name: "my-daemonset",
			}},
		},
	}
	wl := ctrl.getWorkloadRef(pod)
	assert.Equal(t, "my-daemonset", wl.Name)
	assert.Equal(t, "DaemonSet", wl.Kind)
}

func TestGetDaemonSetName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected string
	}{
		{
			name: "pod owned by DaemonSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "DaemonSet",
						Name: "my-daemonset",
					}},
				},
			},
			expected: "my-daemonset",
		},
		{
			name: "pod not owned by DaemonSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "ReplicaSet",
						Name: "my-rs-abc123",
					}},
				},
			},
			expected: "",
		},
		{
			name: "pod with no owner",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := getDaemonSetName(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetStatefulSetName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected string
	}{
		{
			name: "pod owned by StatefulSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "StatefulSet",
						Name: "my-statefulset",
					}},
				},
			},
			expected: "my-statefulset",
		},
		{
			name: "pod not owned by StatefulSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "ReplicaSet",
						Name: "my-rs-abc123",
					}},
				},
			},
			expected: "",
		},
		{
			name: "pod with no owner",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := getStatefulSetName(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetWorkloadRef_StatefulSet(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(&mockPoster{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "StatefulSet",
				Name: "my-statefulset",
			}},
		},
	}
	wl := ctrl.getWorkloadRef(pod)
	assert.Equal(t, "my-statefulset", wl.Name)
	assert.Equal(t, "StatefulSet", wl.Kind)
}

func TestIsNumeric(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input    string
		expected bool
	}{
		{"28485120", true},
		{"0", true},
		{"123456789", true},
		{"", false},
		{"abc", false},
		{"123abc", false},
		{"12-34", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isNumeric(tt.input))
		})
	}
}

func TestGetWorkloadRef_StandaloneJob(t *testing.T) {
	t.Parallel()
	// With nil listers, resolveJobWorkload falls back to standalone Job
	ctrl := newTestController(&mockPoster{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Job",
				Name: "my-standalone-job",
			}},
		},
	}
	wl := ctrl.getWorkloadRef(pod)
	assert.Equal(t, "my-standalone-job", wl.Name)
	assert.Equal(t, "Job", wl.Kind)
}

func TestGetWorkloadRef_NoOwner(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(&mockPoster{})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{},
	}
	wl := ctrl.getWorkloadRef(pod)
	assert.Empty(t, wl.Name)
	assert.Empty(t, wl.Kind)
}

func TestIsTerminalPhase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		phase    corev1.PodPhase
		expected bool
	}{
		{"Succeeded", corev1.PodSucceeded, true},
		{"Failed", corev1.PodFailed, true},
		{"Running", corev1.PodRunning, false},
		{"Pending", corev1.PodPending, false},
		{"Unknown", corev1.PodUnknown, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pod := &corev1.Pod{
				Status: corev1.PodStatus{Phase: tt.phase},
			}
			assert.Equal(t, tt.expected, isTerminalPhase(pod))
		})
	}
}

func TestRun_InformerSyncTimeout(t *testing.T) {
	t.Parallel()
	fakeClient := fake.NewSimpleClientset()
	fakeClient.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// Block until the test context is cancelled.
		<-make(chan struct{})
		return true, nil, nil
	})

	factory := createInformerFactory(fakeClient, "", "")

	ctrl := &Controller{
		clientset:           fakeClient,
		podInformer:         factory.Core().V1().Pods().Informer(),
		deploymentInformer:  factory.Apps().V1().Deployments().Informer(),
		daemonSetInformer:   factory.Apps().V1().DaemonSets().Informer(),
		statefulSetInformer: factory.Apps().V1().StatefulSets().Informer(),
		jobInformer:         factory.Batch().V1().Jobs().Informer(),
		cronJobInformer:     factory.Batch().V1().CronJobs().Informer(),
		workqueue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[PodEvent](),
		),
		apiClient:           &mockPoster{},
		cfg:                 &Config{},
		observedDeployments: amcache.NewExpiring(),
		unknownArtifacts:    amcache.NewExpiring(),
		informerSyncTimeout: 2 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- ctrl.Run(context.Background(), 1)
	}()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out waiting for caches to sync")
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 seconds — informer sync timeout was 2 seconds")
	}
}
