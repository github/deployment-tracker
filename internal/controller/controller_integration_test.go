package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/github/deployment-tracker/internal/metadata"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8smetadata "k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type mockRecordPoster struct {
	mu      sync.Mutex
	records []*deploymentrecord.DeploymentRecord
	err     error // to simulate failures
}

func (m *mockRecordPoster) PostOne(_ context.Context, record *deploymentrecord.DeploymentRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, record)
	return m.err
}

// Helper that allows tests to read captured records safely.
func (m *mockRecordPoster) getRecords() []*deploymentrecord.DeploymentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.records)
}

const testControllerNamespace = "test-controller-ns"

func setup(t *testing.T, onlyNamepsace string, excludeNamespaces string) (*kubernetes.Clientset, *mockRecordPoster) {
	t.Helper()
	testEnv := &envtest.Environment{}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start test environment: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create Kubernetes clientset: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = testEnv.Stop()
	})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testControllerNamespace}}
	_, err = clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	metadataClient, err := k8smetadata.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create Kubernetes metadata client: %v", err)
	}

	metadataAggregator := metadata.NewAggregator(metadataClient)

	ctrl, err := New(
		clientset,
		metadataAggregator,
		onlyNamepsace,
		excludeNamespaces,
		&Config{
			"{{namespace}}/{{deploymentName}}/{{containerName}}",
			"test-logical-env",
			"test physical-env",
			"test-cluster",
			"",
			"",
			"",
			"",
			"",
			"test-org",
		},
	)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}
	mockDeploymentrecord := &mockRecordPoster{}
	ctrl.apiClient = mockDeploymentrecord

	go func() {
		_ = ctrl.Run(ctx, 1)
	}()
	if !cache.WaitForCacheSync(ctx.Done(), ctrl.podInformer.HasSynced) {
		t.Fatal("timed out waiting for informer cache to sync")
	}

	return clientset, mockDeploymentrecord
}

func makeDeployment(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *appsv1.Deployment {
	t.Helper()
	ctx := context.Background()
	labels := map[string]string{"app": name}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
				},
			},
		},
	}
	d, err := clientset.AppsV1().Deployments(namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create Deployment: %v", err)
	}
	return d
}

func makeReplicaSet(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *appsv1.ReplicaSet {
	t.Helper()
	ctx := context.Background()
	labels := map[string]string{"app": name}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
				},
			},
		},
	}
	rs, err := clientset.AppsV1().ReplicaSets(namespace).Create(ctx, replicaSet, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create ReplicaSet: %v", err)
	}
	return rs
}

func makePod(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
		},
	}
	created, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create Pod: %v", err)
	}

	// First set the pod to Pending phase
	created.Status.Phase = corev1.PodPending
	pending, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Pending: %v", err)
	}

	// Then transition to Running
	pending.Status.Phase = corev1.PodRunning
	pending.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "app",
		ImageID: "docker-pullable://nginx@sha256:abc123def456",
	}}
	updated, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, pending, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Running: %v", err)
	}
	return updated
}

func makePodWithInitContainer(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init", Image: "busybox:latest"}},
			Containers:     []corev1.Container{{Name: "app", Image: "nginx:latest"}},
		},
	}
	created, err := clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create Pod: %v", err)
	}

	created.Status.Phase = corev1.PodPending
	pending, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Pending: %v", err)
	}

	pending.Status.Phase = corev1.PodRunning
	pending.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:    "init",
		ImageID: "docker-pullable://busybox@sha256:initdigest789",
	}}
	pending.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "app",
		ImageID: "docker-pullable://nginx@sha256:abc123def456",
	}}
	updated, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, pending, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Running: %v", err)
	}
	return updated
}

func deleteDeployment(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	err := clientset.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("failed to delete Deployment: %v", err)
	}
}

func deleteReplicaSet(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	err := clientset.AppsV1().ReplicaSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("failed to delete ReplicaSet: %v", err)
	}
}

func deletePod(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	err := clientset.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("failed to delete Pod: %v", err)
	}
}

func TestControllerIntegration_KubernetesDeployment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace := "test-controller-ns"
	clientset, mock := setup(t, "", "")

	// Create deployment, replicaset, and pod; expect 1 record
	deployment := makeDeployment(t, clientset, []metav1.OwnerReference{}, namespace, "test-deployment")
	replicaSet := makeReplicaSet(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment.Name,
		UID:        deployment.UID,
	}}, namespace, "test-deployment-123456")
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet.Name,
		UID:        replicaSet.UID,
	}}, namespace, "test-deployment-123456-1")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 1
	}, 3*time.Second, 100*time.Millisecond)
	records := mock.getRecords()
	require.Len(t, records, 1)
	assert.Equal(t, deploymentrecord.StatusDeployed, records[0].Status)

	// Create another pod in replicaset; the dedup cache should prevent a new record as there is only one worker
	// and no risk of multiple works processing before cache is set.
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet.Name,
		UID:        replicaSet.UID,
	}}, namespace, "test-deployment-123456-2")
	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)

	// Delete second pod; still expect 1 record
	deletePod(t, clientset, namespace, "test-deployment-123456-2")
	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)

	// Delete deployment, replicaset, and first pod; expect 2 records
	deleteDeployment(t, clientset, namespace, "test-deployment")
	deleteReplicaSet(t, clientset, namespace, "test-deployment-123456")
	deletePod(t, clientset, namespace, "test-deployment-123456-1")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 2
	}, 3*time.Second, 100*time.Millisecond)
	records = mock.getRecords()
	require.Len(t, records, 2)
	assert.Equal(t, deploymentrecord.StatusDecommissioned, records[1].Status)
}

func TestControllerIntegration_InitContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace := "test-controller-ns"
	clientset, mock := setup(t, "", "")

	// Create deployment, replicaset, and pod with an init container; expect 2 records (one per container)
	deployment := makeDeployment(t, clientset, []metav1.OwnerReference{}, namespace, "init-deployment")
	replicaSet := makeReplicaSet(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment.Name,
		UID:        deployment.UID,
	}}, namespace, "init-deployment-abc123")
	_ = makePodWithInitContainer(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet.Name,
		UID:        replicaSet.UID,
	}}, namespace, "init-deployment-abc123-1")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 2
	}, 3*time.Second, 100*time.Millisecond)
	records := mock.getRecords()
	require.Len(t, records, 2)

	// Both records should be deployed; collect deployment names to verify both containers are recorded
	deploymentNames := make([]string, len(records))
	for i, r := range records {
		assert.Equal(t, deploymentrecord.StatusDeployed, r.Status)
		deploymentNames[i] = r.DeploymentName
	}
	assert.Contains(t, deploymentNames, fmt.Sprintf("%s/init-deployment/app", namespace))
	assert.Contains(t, deploymentNames, fmt.Sprintf("%s/init-deployment/init", namespace))

	// Delete deployment, replicaset, and pod; expect 2 more decommissioned records (one per container)
	deleteDeployment(t, clientset, namespace, "init-deployment")
	deleteReplicaSet(t, clientset, namespace, "init-deployment-abc123")
	deletePod(t, clientset, namespace, "init-deployment-abc123-1")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 4
	}, 3*time.Second, 100*time.Millisecond)
	records = mock.getRecords()
	require.Len(t, records, 4)

	decommissionedNames := make([]string, 0, 2)
	for _, r := range records[2:] {
		assert.Equal(t, deploymentrecord.StatusDecommissioned, r.Status)
		decommissionedNames = append(decommissionedNames, r.DeploymentName)
	}
	assert.Contains(t, decommissionedNames, fmt.Sprintf("%s/init-deployment/app", namespace))
	assert.Contains(t, decommissionedNames, fmt.Sprintf("%s/init-deployment/init", namespace))
}

func TestControllerIntegration_OnlyWatchOneNamespace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace1 := "namespace1"
	namespace2 := "namespace2"
	clientset, mock := setup(t, namespace1, "")

	ns1 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace1}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), ns1, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}
	ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace2}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Make new deployment in namespace1; expect 1 record
	deployment1 := makeDeployment(t, clientset, []metav1.OwnerReference{}, namespace1, "init-deployment")
	replicaSet1 := makeReplicaSet(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment1.Name,
		UID:        deployment1.UID,
	}}, namespace1, "init-deployment-abc123")
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet1.Name,
		UID:        replicaSet1.UID,
	}}, namespace1, "init-deployment-abc123-1")
	require.Eventually(t, func() bool {
		return len(mock.getRecords()) == 1
	}, 3*time.Second, 100*time.Millisecond)

	// Make new deployment in namespace2; expect no new records
	deployment2 := makeDeployment(t, clientset, []metav1.OwnerReference{}, namespace2, "init-deployment")
	replicaSet2 := makeReplicaSet(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment2.Name,
		UID:        deployment2.UID,
	}}, namespace2, "init-deployment-abc123")
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet2.Name,
		UID:        replicaSet2.UID,
	}}, namespace2, "init-deployment-abc123-1")
	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)
}

func TestControllerIntegration_ExcludeNamespaces(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace1 := "namespace1"
	namespace2 := "namespace2"
	namespace3 := "namespace3"
	clientset, mock := setup(t, "", fmt.Sprintf("%s,%s", namespace2, namespace3))

	ns1 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace1}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), ns1, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}
	ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace2}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns2, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}
	ns3 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace3}}
	_, err = clientset.CoreV1().Namespaces().Create(context.Background(), ns3, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Make new deployment in namespace1; expect 1 record
	deployment1 := makeDeployment(t, clientset, []metav1.OwnerReference{}, namespace1, "init-deployment")
	replicaSet1 := makeReplicaSet(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment1.Name,
		UID:        deployment1.UID,
	}}, namespace1, "init-deployment-abc123")
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet1.Name,
		UID:        replicaSet1.UID,
	}}, namespace1, "init-deployment-abc123-1")
	require.Eventually(t, func() bool {
		return len(mock.getRecords()) == 1
	}, 3*time.Second, 100*time.Millisecond)

	// Make new deployment in namespace2; expect no new records
	deployment2 := makeDeployment(t, clientset, []metav1.OwnerReference{}, namespace2, "init-deployment")
	replicaSet2 := makeReplicaSet(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment2.Name,
		UID:        deployment2.UID,
	}}, namespace2, "init-deployment-abc123")
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet2.Name,
		UID:        replicaSet2.UID,
	}}, namespace2, "init-deployment-abc123-1")

	// Make new deployment in namespace 3; expect no new records
	deployment3 := makeDeployment(t, clientset, []metav1.OwnerReference{}, namespace3, "init-deployment")
	replicaSet3 := makeReplicaSet(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment3.Name,
		UID:        deployment3.UID,
	}}, namespace3, "init-deployment-abc123")
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet3.Name,
		UID:        replicaSet3.UID,
	}}, namespace3, "init-deployment-abc123-1")

	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)
}
