package workload

import (
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	appslisters "k8s.io/client-go/listers/apps/v1"
	batchlisters "k8s.io/client-go/listers/batch/v1"
)

// Identity describes the top-level workload that owns a pod.
type Identity struct {
	Name string
	Kind string // "Deployment", "DaemonSet", "StatefulSet", "CronJob", or "Job"
}

// Resolver resolves pod ownership and checks workload liveness
// using Kubernetes informer caches.
type Resolver struct {
	deploymentLister  appslisters.DeploymentLister
	daemonSetLister   appslisters.DaemonSetLister
	statefulSetLister appslisters.StatefulSetLister
	jobLister         batchlisters.JobLister
	cronJobLister     batchlisters.CronJobLister
}

// NewResolver creates a new Resolver with the given Kubernetes listers.
func NewResolver(
	deploymentLister appslisters.DeploymentLister,
	daemonSetLister appslisters.DaemonSetLister,
	statefulSetLister appslisters.StatefulSetLister,
	jobLister batchlisters.JobLister,
	cronJobLister batchlisters.CronJobLister,
) *Resolver {
	return &Resolver{
		deploymentLister:  deploymentLister,
		daemonSetLister:   daemonSetLister,
		statefulSetLister: statefulSetLister,
		jobLister:         jobLister,
		cronJobLister:     cronJobLister,
	}
}

// Resolve returns the top-level workload that owns the pod.
func (r *Resolver) Resolve(pod *corev1.Pod) Identity {
	// Check for Deployment (via ReplicaSet)
	if dn := GetDeploymentName(pod); dn != "" {
		return Identity{Name: dn, Kind: "Deployment"}
	}

	// Check for DaemonSet (direct ownership)
	if dsn := GetDaemonSetName(pod); dsn != "" {
		return Identity{Name: dsn, Kind: "DaemonSet"}
	}

	// Check for StatefulSet (direct ownership)
	if ssn := GetStatefulSetName(pod); ssn != "" {
		return Identity{Name: ssn, Kind: "StatefulSet"}
	}

	// Check for Job
	jobName := GetJobOwnerName(pod)
	if jobName == "" {
		return Identity{}
	}

	return r.resolveJobWorkload(pod.Namespace, jobName)
}

// IsActive checks if the parent workload for a pod still exists
// in the local informer cache.
func (r *Resolver) IsActive(namespace string, ref Identity) bool {
	switch ref.Kind {
	case "Deployment":
		if r.deploymentLister == nil {
			return false
		}
		return r.deploymentExists(namespace, ref.Name)
	case "DaemonSet":
		if r.daemonSetLister == nil {
			return false
		}
		return r.daemonSetExists(namespace, ref.Name)
	case "StatefulSet":
		if r.statefulSetLister == nil {
			return false
		}
		return r.statefulSetExists(namespace, ref.Name)
	case "CronJob":
		if r.cronJobLister == nil {
			return false
		}
		return r.cronJobExists(namespace, ref.Name)
	case "Job":
		if r.jobLister == nil {
			return false
		}
		return r.jobExists(namespace, ref.Name)
	default:
		return false
	}
}

// resolveJobWorkload determines whether a Job is owned by a CronJob or is standalone.
func (r *Resolver) resolveJobWorkload(namespace, jobName string) Identity {
	// Try to look up the Job to check for CronJob ownership
	if r.jobLister != nil {
		job, err := r.jobLister.Jobs(namespace).Get(jobName)
		if err == nil {
			for _, owner := range job.OwnerReferences {
				if owner.Kind == "CronJob" {
					return Identity{Name: owner.Name, Kind: "CronJob"}
				}
			}
			return Identity{Name: jobName, Kind: "Job"}
		}
	}

	// Job not found in cache - try CronJob name derivation as fallback.
	// CronJob-created Jobs follow the naming pattern: <cronjob-name>-<unix-timestamp>
	// where the suffix is always numeric. We validate the suffix is all digits to
	// reduce false matches from standalone Jobs that coincidentally share a prefix
	// with an existing CronJob. A residual false positive is still possible if a
	// standalone Job is named exactly <cronjob>-<digits>, but the primary path
	// (checking Job OwnerReferences) handles the common case; this fallback only
	// fires when the Job has already been garbage-collected.
	if r.cronJobLister != nil {
		lastDash := strings.LastIndex(jobName, "-")
		if lastDash > 0 {
			suffix := jobName[lastDash+1:]
			if isNumeric(suffix) {
				potentialCronJobName := jobName[:lastDash]
				if r.cronJobExists(namespace, potentialCronJobName) {
					return Identity{Name: potentialCronJobName, Kind: "CronJob"}
				}
			}
		}
	}

	// Standalone Job (possibly already deleted)
	return Identity{Name: jobName, Kind: "Job"}
}

func (r *Resolver) deploymentExists(namespace, name string) bool {
	_, err := r.deploymentLister.Deployments(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if deployment exists in cache, assuming it does",
			"namespace", namespace,
			"deployment", name,
			"error", err,
		)
		return true
	}
	return true
}

func (r *Resolver) jobExists(namespace, name string) bool {
	_, err := r.jobLister.Jobs(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if job exists in cache, assuming it does",
			"namespace", namespace,
			"job", name,
			"error", err,
		)
		return true
	}
	return true
}

func (r *Resolver) daemonSetExists(namespace, name string) bool {
	_, err := r.daemonSetLister.DaemonSets(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if daemonset exists in cache, assuming it does",
			"namespace", namespace,
			"daemonset", name,
			"error", err,
		)
		return true
	}
	return true
}

func (r *Resolver) statefulSetExists(namespace, name string) bool {
	_, err := r.statefulSetLister.StatefulSets(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if statefulset exists in cache, assuming it does",
			"namespace", namespace,
			"statefulset", name,
			"error", err,
		)
		return true
	}
	return true
}

func (r *Resolver) cronJobExists(namespace, name string) bool {
	_, err := r.cronJobLister.CronJobs(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return false
		}
		slog.Warn("Failed to check if cronjob exists in cache, assuming it does",
			"namespace", namespace,
			"cronjob", name,
			"error", err,
		)
		return true
	}
	return true
}

// HasSupportedOwner returns true if the pod is owned by a supported
// workload controller (ReplicaSet for Deployments, DaemonSet, StatefulSet, or Job for Jobs/CronJobs).
func HasSupportedOwner(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" || owner.Kind == "DaemonSet" || owner.Kind == "StatefulSet" || owner.Kind == "Job" {
			return true
		}
	}
	return false
}

// IsTerminalPhase returns true if the pod has reached a terminal phase
// (Succeeded or Failed). Used to catch short-lived Job pods that complete
// before the controller observes them in the Running phase.
func IsTerminalPhase(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// GetDeploymentName returns the deployment name for a pod, if it belongs
// to one.
func GetDeploymentName(pod *corev1.Pod) string {
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

// GetJobOwnerName returns the Job name from the pod's owner references,
// if the pod is owned by a Job.
func GetJobOwnerName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" {
			return owner.Name
		}
	}
	return ""
}

// GetDaemonSetName returns the DaemonSet name for a pod, if it belongs
// to one. DaemonSet pods are owned directly by the DaemonSet.
func GetDaemonSetName(pod *corev1.Pod) string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return owner.Name
		}
	}
	return ""
}

// GetStatefulSetName returns the StatefulSet name for a pod, if it belongs
// to one. StatefulSet pods are owned directly by the StatefulSet.
func GetStatefulSetName(pod *corev1.Pod) string {
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
