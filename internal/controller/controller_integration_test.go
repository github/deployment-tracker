package controller

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/github/deployment-tracker/internal/metadata"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	k8smetadata "k8s.io/client-go/metadata"
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

// Helper that allows tests to read captured records safely
func (m *mockRecordPoster) getRecords() []*deploymentrecord.DeploymentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.records)
}

func setup(t *testing.T, namespace string) (*envtest.Environment, context.CancelFunc, *kubernetes.Clientset, *mockRecordPoster) {
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

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
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
		"",
		"",
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

	go ctrl.Run(ctx, 1)
	time.Sleep(1 * time.Second)

	return testEnv, cancel, clientset, mockDeploymentrecord
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

	created.Status.Phase = corev1.PodRunning
	created.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "app",
		ImageID: "docker-pullable://nginx@sha256:abc123def456",
	}}
	updated, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status: %v", err)
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

// pollForRecords polls until the mock has at least minCount records, then returns them.
func pollForRecords(t *testing.T, mock *mockRecordPoster, minCount int, timeout time.Duration) []*deploymentrecord.DeploymentRecord {
	t.Helper()
	deadline := time.After(timeout)
	for {
		records := mock.getRecords()
		if len(records) >= minCount {
			return records
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for at least %d records, got %d", minCount, len(records))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// assertNoNewRecords polls for the given duration and fails if the record count deviates from expectedCount.
func assertNoNewRecords(t *testing.T, mock *mockRecordPoster, expectedCount int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			if got := len(mock.getRecords()); got != expectedCount {
				t.Fatalf("expected %d records, got %d", expectedCount, got)
			}
			return
		case <-time.After(100 * time.Millisecond):
			if got := len(mock.getRecords()); got != expectedCount {
				t.Fatalf("expected %d records, got %d", expectedCount, got)
			}
		}
	}
}

func TestControllerIntegration_KubernetesDeployment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	namespace := "test-namespace"
	testEnv, cancel, clientset, mock := setup(t, namespace)
	defer func(testEnv *envtest.Environment, cancelFunc context.CancelFunc) {
		_ = testEnv.Stop()
		cancel()
	}(testEnv, cancel)

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

	records := pollForRecords(t, mock, 1, 5*time.Second)
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].Status != deploymentrecord.StatusDeployed {
		t.Errorf("expected %s, got %s", deploymentrecord.StatusDeployed, records[0].Status)
	}

	// Create another pod in replicaset; the dedup cache should prevent a new record as there is only one worker
	// and no risk of multiple works processing before cache is set.
	_ = makePod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       replicaSet.Name,
		UID:        replicaSet.UID,
	}}, namespace, "test-deployment-123456-2")
	assertNoNewRecords(t, mock, 1, 5*time.Second)

	// Delete second pod; still expect 1 record
	deletePod(t, clientset, namespace, "test-deployment-123456-2")
	assertNoNewRecords(t, mock, 1, 5*time.Second)

	// Delete deployment, replicaset, and first pod; expect 2 records
	deleteDeployment(t, clientset, namespace, "test-deployment")
	deleteReplicaSet(t, clientset, namespace, "test-deployment-123456")
	deletePod(t, clientset, namespace, "test-deployment-123456-1")

	records = pollForRecords(t, mock, 2, 5*time.Second)
	if len(records) != 2 {
		t.Fatalf("expected 2 records after deletion, got %d", len(records))
	}
	if records[1].Status != deploymentrecord.StatusDecommissioned {
		t.Errorf("expected second record to be %s, got %s", deploymentrecord.StatusDecommissioned, records[1].Status)
	}
}
