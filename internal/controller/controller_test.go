package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/github/deployment-tracker/internal/workload"
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

// mockResolver is a test double for the workloadResolver interface.
type mockResolver struct{}

func (*mockResolver) Resolve(_ *corev1.Pod) workload.Identity {
	return workload.Identity{}
}

func (*mockResolver) IsActive(_ string, _ workload.Identity) bool {
	return false
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
		workloadResolver:    &mockResolver{},
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

func TestRun_InformerSyncTimeout(t *testing.T) {
	t.Parallel()
	fakeClient := fake.NewSimpleClientset()
	blocker := make(chan struct{})
	fakeClient.PrependReactor("list", "*", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		// Block until the test completes.
		<-blocker
		return true, nil, errors.New("fail")
	})
	defer close(blocker)

	factory := createInformerFactory(fakeClient, "", "")

	// Ensure the informers are registered with the factory by accessing them
	factory.Core().V1().Pods().Informer()
	factory.Apps().V1().Deployments().Informer()
	factory.Apps().V1().DaemonSets().Informer()
	factory.Apps().V1().StatefulSets().Informer()
	factory.Batch().V1().Jobs().Informer()
	factory.Batch().V1().CronJobs().Informer()

	ctrl := &Controller{
		informerFactory: factory,
		podInformer:     factory.Core().V1().Pods().Informer(),
		workqueue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[PodEvent](),
		),
		workloadResolver:    &mockResolver{},
		apiClient:           &mockPoster{},
		cfg:                 &Config{},
		observedDeployments: amcache.NewExpiring(),
		unknownArtifacts:    amcache.NewExpiring(),
		informerSyncTimeout: 2 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- ctrl.Run(ctx, 1)
	}()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out waiting for caches to sync")
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5 seconds — informer sync timeout was 2 seconds")
	}
}
