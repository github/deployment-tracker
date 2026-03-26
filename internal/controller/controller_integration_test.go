//go:build integration_test

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/github/deployment-tracker/internal/metadata"
	"github.com/github/deployment-tracker/pkg/deploymentrecord"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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

func setup(t *testing.T, onlyNamespace string, excludeNamespaces string) (*kubernetes.Clientset, *mockRecordPoster) {
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

	if onlyNamespace != "" {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: onlyNamespace}}
		_, err = clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create onlyNamespace: %v", err)
		}
	}

	if excludeNamespaces != "" {
		for _, nsName := range strings.Split(excludeNamespaces, ",") {
			ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
			_, err = clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to create excludeNamespace %s: %v", nsName, err)
			}
		}
	}

	metadataClient, err := k8smetadata.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create Kubernetes metadata client: %v", err)
	}

	metadataAggregator := metadata.NewAggregator(metadataClient)

	ctrl, err := New(
		clientset,
		metadataAggregator,
		onlyNamespace,
		excludeNamespaces,
		&Config{
			Template:            "{{namespace}}/{{deploymentName}}/{{containerName}}",
			LogicalEnvironment:  "test-logical-env",
			PhysicalEnvironment: "test-physical-env",
			Cluster:             "test-cluster",
			Organization:        "test-org",
		},
	)
	if err != nil {
		t.Fatalf("failed to create controller: %v", err)
	}
	mockDeploymentRecordPoster := &mockRecordPoster{}
	ctrl.apiClient = mockDeploymentRecordPoster

	go func() {
		_ = ctrl.Run(ctx, 1)
	}()
	if !cache.WaitForCacheSync(ctx.Done(),
		ctrl.podInformer.HasSynced,
		ctrl.deploymentInformer.HasSynced,
		ctrl.daemonSetInformer.HasSynced,
		ctrl.statefulSetInformer.HasSynced,
		ctrl.jobInformer.HasSynced,
		ctrl.cronJobInformer.HasSynced,
	) {
		t.Fatal("timed out waiting for informer cache to sync")
	}

	return clientset, mockDeploymentRecordPoster
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

func makeDaemonSet(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) *appsv1.DaemonSet {
	t.Helper()
	ctx := context.Background()
	labels := map[string]string{"app": name}
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "agent", Image: "fluentd:latest"}},
				},
			},
		},
	}
	created, err := clientset.AppsV1().DaemonSets(namespace).Create(ctx, ds, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create DaemonSet: %v", err)
	}
	return created
}

func makeDaemonSetPod(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "fluentd:latest"}},
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
	pending.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "agent",
		ImageID: "docker-pullable://fluentd@sha256:dsdigest123",
	}}
	updated, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, pending, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Running: %v", err)
	}
	return updated
}

func deleteDaemonSet(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	err := clientset.AppsV1().DaemonSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("failed to delete DaemonSet: %v", err)
	}
}

func makeCronJob(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) *batchv1.CronJob {
	t.Helper()
	ctx := context.Background()
	cronJob := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: "*/5 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers:    []corev1.Container{{Name: "worker", Image: "busybox:latest"}},
							RestartPolicy: corev1.RestartPolicyNever,
						},
					},
				},
			},
		},
	}
	created, err := clientset.BatchV1().CronJobs(namespace).Create(ctx, cronJob, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create CronJob: %v", err)
	}
	return created
}

func makeJob(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *batchv1.Job {
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "worker", Image: "busybox:latest"}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}
	created, err := clientset.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create Job: %v", err)
	}
	return created
}

func makeJobPod(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "worker", Image: "busybox:latest"}},
			RestartPolicy: corev1.RestartPolicyNever,
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
	pending.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "worker",
		ImageID: "docker-pullable://busybox@sha256:jobdigest789",
	}}
	updated, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, pending, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Running: %v", err)
	}
	return updated
}

// makeCompletedJobPod creates a Job-owned pod that transitions directly from
// Pending to Succeeded, skipping the Running phase. This simulates a very
// short-lived Job (e.g. sub-second container execution) where the kubelet
// reports the final status without an intermediate Running update.
func makeCompletedJobPod(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			Containers:    []corev1.Container{{Name: "worker", Image: "busybox:latest"}},
			RestartPolicy: corev1.RestartPolicyNever,
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

	// Transition directly to Succeeded without passing through Running
	pending.Status.Phase = corev1.PodSucceeded
	pending.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "worker",
		ImageID: "docker-pullable://busybox@sha256:jobdigest789",
	}}
	updated, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, pending, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Succeeded: %v", err)
	}
	return updated
}

func deleteCronJob(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	err := clientset.BatchV1().CronJobs(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("failed to delete CronJob: %v", err)
	}
}

func deleteJob(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	propagation := metav1.DeletePropagationBackground
	err := clientset.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if err != nil {
		t.Fatalf("failed to delete Job: %v", err)
	}
}

func TestControllerIntegration_KubernetesDeploymentLifecycle(t *testing.T) {
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
	// and no risk of multiple workers processing before cache is set.
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

	// Make invalid namespaces
	ns2 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace2}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), ns2, metav1.CreateOptions{})
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

	// Make valid namespace
	ns1 := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace1}}
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), ns1, metav1.CreateOptions{})
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

func TestControllerIntegration_StandaloneJobLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace := "test-controller-ns"
	clientset, mock := setup(t, "", "")

	// Create a standalone Job and its pod; expect 1 record
	job := makeJob(t, clientset, []metav1.OwnerReference{}, namespace, "standalone-job")
	_ = makeJobPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}}, namespace, "standalone-job-pod-1")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 1
	}, 3*time.Second, 100*time.Millisecond)
	records := mock.getRecords()
	require.Len(t, records, 1)
	assert.Equal(t, deploymentrecord.StatusDeployed, records[0].Status)
	assert.Equal(t, fmt.Sprintf("%s/standalone-job/worker", namespace), records[0].DeploymentName)

	// Delete the pod while the Job still exists; should not decommission (like scale-down)
	deletePod(t, clientset, namespace, "standalone-job-pod-1")
	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)

	// Create a new pod for the same job and then delete both.
	// The second pod has the same deployment name and digest, so the dedup
	// cache suppresses a duplicate CREATED record (2-minute TTL).
	_ = makeJobPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}}, namespace, "standalone-job-pod-2")

	// Delete the Job first, then the pod manually (envtest has no garbage
	// collector, so Background propagation does not cascade pod deletion).
	deleteJob(t, clientset, namespace, "standalone-job")
	deletePod(t, clientset, namespace, "standalone-job-pod-2")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 2
	}, 3*time.Second, 100*time.Millisecond)
	records = mock.getRecords()
	require.Len(t, records, 2)
	assert.Equal(t, deploymentrecord.StatusDecommissioned, records[1].Status)
	assert.Equal(t, fmt.Sprintf("%s/standalone-job/worker", namespace), records[1].DeploymentName)
}

func TestControllerIntegration_ShortLivedJobCaughtOnCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace := "test-controller-ns"
	clientset, mock := setup(t, "", "")

	// Create a Job and a pod that goes directly from Pending to Succeeded
	// (simulating a sub-second Job that completes before the Running phase is observed).
	job := makeJob(t, clientset, []metav1.OwnerReference{}, namespace, "fast-job")
	_ = makeCompletedJobPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}}, namespace, "fast-job-pod-1")

	// The controller should still catch it via the terminal phase handler
	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 1
	}, 3*time.Second, 100*time.Millisecond)
	records := mock.getRecords()
	require.Len(t, records, 1)
	assert.Equal(t, deploymentrecord.StatusDeployed, records[0].Status)
	assert.Equal(t, fmt.Sprintf("%s/fast-job/worker", namespace), records[0].DeploymentName)
}

func TestControllerIntegration_CronJobLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace := "test-controller-ns"
	clientset, mock := setup(t, "", "")

	// Create a CronJob, a Job owned by it, and a pod; expect 1 record
	cronJob := makeCronJob(t, clientset, namespace, "my-cronjob")
	job := makeJob(t, clientset, []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "CronJob",
		Name:       cronJob.Name,
		UID:        cronJob.UID,
	}}, namespace, "my-cronjob-28485120")
	_ = makeJobPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}}, namespace, "my-cronjob-28485120-pod-1")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 1
	}, 3*time.Second, 100*time.Millisecond)
	records := mock.getRecords()
	require.Len(t, records, 1)
	assert.Equal(t, deploymentrecord.StatusDeployed, records[0].Status)
	// The deployment name should use the CronJob name, not the Job name
	assert.Equal(t, fmt.Sprintf("%s/my-cronjob/worker", namespace), records[0].DeploymentName)

	// Delete the Job and pod while CronJob still exists; should not decommission
	deleteJob(t, clientset, namespace, "my-cronjob-28485120")
	deletePod(t, clientset, namespace, "my-cronjob-28485120-pod-1")
	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)

	// Now delete the CronJob and create a new job+pod to simulate final cleanup
	deleteCronJob(t, clientset, namespace, "my-cronjob")

	// Create another job+pod that gets cleaned up after CronJob deletion.
	// The dedup cache suppresses a new CREATED since the deployment name
	// and digest match the earlier record (2-minute TTL).
	job2 := makeJob(t, clientset, []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "CronJob",
		Name:       cronJob.Name,
		UID:        cronJob.UID,
	}}, namespace, "my-cronjob-28485240")
	pod2 := makeJobPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job2.Name,
		UID:        job2.UID,
	}}, namespace, "my-cronjob-28485240-pod-1")

	// Delete the pod first so the Job is still in the informer cache
	// when the delete event is processed — this allows resolveJobWorkload
	// to find the CronJob owner via the Job's OwnerReferences.
	// Then delete the Job for cleanup.
	deletePod(t, clientset, namespace, pod2.Name)
	deleteJob(t, clientset, namespace, "my-cronjob-28485240")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 2
	}, 3*time.Second, 100*time.Millisecond)
	records = mock.getRecords()
	require.Len(t, records, 2)
	assert.Equal(t, deploymentrecord.StatusDecommissioned, records[1].Status)
	assert.Equal(t, fmt.Sprintf("%s/my-cronjob/worker", namespace), records[1].DeploymentName)
}

func TestControllerIntegration_DaemonSetLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace := "test-controller-ns"
	clientset, mock := setup(t, "", "")

	// Create a DaemonSet and a pod owned by it; expect 1 record
	ds := makeDaemonSet(t, clientset, namespace, "logging-agent")
	_ = makeDaemonSetPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "DaemonSet",
		Name:       ds.Name,
		UID:        ds.UID,
	}}, namespace, "logging-agent-node1")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 1
	}, 3*time.Second, 100*time.Millisecond)
	records := mock.getRecords()
	require.Len(t, records, 1)
	assert.Equal(t, deploymentrecord.StatusDeployed, records[0].Status)
	assert.Equal(t, fmt.Sprintf("%s/logging-agent/agent", namespace), records[0].DeploymentName)

	// Delete the pod while DaemonSet still exists; should NOT decommission
	deletePod(t, clientset, namespace, "logging-agent-node1")
	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)

	// Create another pod, then delete both the DaemonSet and pod.
	// The dedup cache suppresses a new CREATED since the deployment name
	// and digest match the earlier record (2-minute TTL).
	pod2 := makeDaemonSetPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "DaemonSet",
		Name:       ds.Name,
		UID:        ds.UID,
	}}, namespace, "logging-agent-node2")

	deleteDaemonSet(t, clientset, namespace, "logging-agent")
	deletePod(t, clientset, namespace, pod2.Name)

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 2
	}, 3*time.Second, 100*time.Millisecond)
	records = mock.getRecords()
	require.Len(t, records, 2)
	assert.Equal(t, deploymentrecord.StatusDecommissioned, records[1].Status)
	assert.Equal(t, fmt.Sprintf("%s/logging-agent/agent", namespace), records[1].DeploymentName)
}

func makeStatefulSet(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) *appsv1.StatefulSet {
	t.Helper()
	ctx := context.Background()
	labels := map[string]string{"app": name}
	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "db", Image: "postgres:latest"}},
				},
			},
		},
	}
	created, err := clientset.AppsV1().StatefulSets(namespace).Create(ctx, ss, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create StatefulSet: %v", err)
	}
	return created
}

func makeStatefulSetPod(t *testing.T, clientset *kubernetes.Clientset, owners []metav1.OwnerReference, namespace, name string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "db", Image: "postgres:latest"}},
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
	pending.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:    "db",
		ImageID: "docker-pullable://postgres@sha256:ssdigest456",
	}}
	updated, err := clientset.CoreV1().Pods(namespace).UpdateStatus(ctx, pending, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update Pod status to Running: %v", err)
	}
	return updated
}

func deleteStatefulSet(t *testing.T, clientset *kubernetes.Clientset, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	err := clientset.AppsV1().StatefulSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("failed to delete StatefulSet: %v", err)
	}
}

func TestControllerIntegration_StatefulSetLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	t.Parallel()
	namespace := "test-controller-ns"
	clientset, mock := setup(t, "", "")

	// Create a StatefulSet and a pod owned by it; expect 1 record
	ss := makeStatefulSet(t, clientset, namespace, "my-db")
	_ = makeStatefulSetPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Name:       ss.Name,
		UID:        ss.UID,
	}}, namespace, "my-db-0")

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 1
	}, 3*time.Second, 100*time.Millisecond)
	records := mock.getRecords()
	require.Len(t, records, 1)
	assert.Equal(t, deploymentrecord.StatusDeployed, records[0].Status)
	assert.Equal(t, fmt.Sprintf("%s/my-db/db", namespace), records[0].DeploymentName)

	// Delete the pod while StatefulSet still exists; should NOT decommission
	deletePod(t, clientset, namespace, "my-db-0")
	require.Never(t, func() bool {
		return len(mock.getRecords()) != 1
	}, 3*time.Second, 100*time.Millisecond)

	// Create another pod, then delete both the StatefulSet and pod.
	// The dedup cache suppresses a new CREATED since the deployment name
	// and digest match the earlier record (2-minute TTL).
	pod2 := makeStatefulSetPod(t, clientset, []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "StatefulSet",
		Name:       ss.Name,
		UID:        ss.UID,
	}}, namespace, "my-db-1")

	deleteStatefulSet(t, clientset, namespace, "my-db")
	deletePod(t, clientset, namespace, pod2.Name)

	require.Eventually(t, func() bool {
		return len(mock.getRecords()) >= 2
	}, 3*time.Second, 100*time.Millisecond)
	records = mock.getRecords()
	require.Len(t, records, 2)
	assert.Equal(t, deploymentrecord.StatusDecommissioned, records[1].Status)
	assert.Equal(t, fmt.Sprintf("%s/my-db/db", namespace), records[1].DeploymentName)
}
