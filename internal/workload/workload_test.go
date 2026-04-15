package workload

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
			result := HasSupportedOwner(tt.pod)
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
			result := GetJobOwnerName(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetDeploymentName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected string
	}{
		{
			name: "pod owned by ReplicaSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "ReplicaSet",
						Name: "my-deployment-abc123",
					}},
				},
			},
			expected: "my-deployment",
		},
		{
			name: "pod not owned by ReplicaSet",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{{
						Kind: "DaemonSet",
						Name: "my-ds",
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
			result := GetDeploymentName(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
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
			result := GetDaemonSetName(tt.pod)
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
			result := GetStatefulSetName(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
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
			assert.Equal(t, tt.expected, IsTerminalPhase(pod))
		})
	}
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

func TestResolve_Deployment(t *testing.T) {
	t.Parallel()
	r := &Resolver{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet",
				Name: "my-deployment-abc123",
			}},
		},
	}
	ref := r.Resolve(pod)
	assert.Equal(t, "my-deployment", ref.Name)
	assert.Equal(t, "Deployment", ref.Kind)
}

func TestResolve_DaemonSet(t *testing.T) {
	t.Parallel()
	r := &Resolver{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "DaemonSet",
				Name: "my-daemonset",
			}},
		},
	}
	ref := r.Resolve(pod)
	assert.Equal(t, "my-daemonset", ref.Name)
	assert.Equal(t, "DaemonSet", ref.Kind)
}

func TestResolve_StatefulSet(t *testing.T) {
	t.Parallel()
	r := &Resolver{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "StatefulSet",
				Name: "my-statefulset",
			}},
		},
	}
	ref := r.Resolve(pod)
	assert.Equal(t, "my-statefulset", ref.Name)
	assert.Equal(t, "StatefulSet", ref.Kind)
}

func TestResolve_StandaloneJob(t *testing.T) {
	t.Parallel()
	// With nil listers, resolveJobWorkload falls back to standalone Job
	r := &Resolver{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Job",
				Name: "my-standalone-job",
			}},
		},
	}
	ref := r.Resolve(pod)
	assert.Equal(t, "my-standalone-job", ref.Name)
	assert.Equal(t, "Job", ref.Kind)
}

func TestResolve_NoOwner(t *testing.T) {
	t.Parallel()
	r := &Resolver{}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{},
	}
	ref := r.Resolve(pod)
	assert.Empty(t, ref.Name)
	assert.Empty(t, ref.Kind)
}
