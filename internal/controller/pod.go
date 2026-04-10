package controller

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/github/deployment-tracker/pkg/ociutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

// createInformerFactory creates a shared informer factory with the given resync period.
// If excludeNamespaces is non-empty, it will exclude those namespaces from being watched.
// If namespace is non-empty, it will only watch that namespace.
func createInformerFactory(clientset kubernetes.Interface, namespace string, excludeNamespaces string) informers.SharedInformerFactory {
	var factory informers.SharedInformerFactory
	switch {
	case namespace != "":
		slog.Info("Namespace to watch",
			"namespace",
			namespace,
		)
		factory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Second,
			informers.WithNamespace(namespace),
		)
	case excludeNamespaces != "":
		seenNamespaces := make(map[string]bool)
		fieldSelectorParts := make([]string, 0)

		for _, ns := range strings.Split(excludeNamespaces, ",") {
			ns = strings.TrimSpace(ns)
			if ns != "" && !seenNamespaces[ns] {
				seenNamespaces[ns] = true
				fieldSelectorParts = append(fieldSelectorParts, fmt.Sprintf("metadata.namespace!=%s", ns))
			}
		}

		slog.Info("Excluding namespaces from watch",
			"field_selector",
			strings.Join(fieldSelectorParts, ","),
		)
		tweakListOptions := func(options *metav1.ListOptions) {
			options.FieldSelector = strings.Join(fieldSelectorParts, ",")
		}

		factory = informers.NewSharedInformerFactoryWithOptions(
			clientset,
			30*time.Second,
			informers.WithTweakListOptions(tweakListOptions),
		)
	default:
		factory = informers.NewSharedInformerFactory(clientset,
			30*time.Second,
		)
	}

	return factory
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

// hasSupportedOwner returns true if the pod is owned by a supported
// workload controller (ReplicaSet for Deployments, DaemonSet, StatefulSet, or Job for Jobs/CronJobs).
func hasSupportedOwner(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" || owner.Kind == "DaemonSet" || owner.Kind == "StatefulSet" || owner.Kind == "Job" {
			return true
		}
	}
	return false
}

// isTerminalPhase returns true if the pod has reached a terminal phase
// (Succeeded or Failed). Used to catch short-lived Job pods that complete
// before the controller observes them in the Running phase.
func isTerminalPhase(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// getDeploymentName returns the deployment name for a pod, if it belongs
// to one.
func getDeploymentName(pod *corev1.Pod) string {
	// Pods created by Deployments are owned by ReplicaSets
	// The ReplicaSet name follows the pattern: <deployment-name>-<hash>
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" {
			// Extract deployment name by removing the hash suffix
			// ReplicaSet name format: <deployment-name>-<hash>
			rsName := owner.Name
			lastDash := strings.LastIndex(rsName, "-")
			if lastDash > 0 {
				return rsName[:lastDash]
			}
			return rsName
		}
	}
	return ""
}

// getJobOwnerName returns the Job name from the pod's owner references,
// if the pod is owned by a Job.
func getJobOwnerName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" {
			return owner.Name
		}
	}
	return ""
}

// getDaemonSetName returns the DaemonSet name for a pod, if it belongs
// to one. DaemonSet pods are owned directly by the DaemonSet.
func getDaemonSetName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return owner.Name
		}
	}
	return ""
}

// getStatefulSetName returns the StatefulSet name for a pod, if it belongs
// to one. StatefulSet pods are owned directly by the StatefulSet.
func getStatefulSetName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "StatefulSet" {
			return owner.Name
		}
	}
	return ""
}

// isNumeric returns true if s is non-empty and consists entirely of ASCII digits.
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
