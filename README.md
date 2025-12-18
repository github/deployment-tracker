# Deployment Tracker

> [!CAUTION]
> This project is in an early preview state and contains experimental
> code. It is under active development and not ready for production
> use. Breaking changes are likely, and stability or security is not
> guaranteed. Use at your own risk.

A Kubernetes controller that monitors pod lifecycles and uploads
deployment records to GitHub's artifact metadata API.

> [!IMPORTANT]
> For the correlation to work in the backend, container images must be
> built with [GitHub Artifact
> Attestations](https://docs.github.com/en/actions/concepts/security/artifact-attestations).

## Features

- **Informer-based controller**: Uses Kubernetes SharedInformers for
  efficient, reliable pod watching
- **Work queue with retries**: Rate-limited work queue with automatic
  retries on failure
- **Real-time tracking**: Sends deployment records when pods are
  created or deleted
- **Graceful shutdown**: Properly drains work queue before terminating

## How It Works

1. The controller watches for pod events using a Kubernetes
   SharedInformer
2. When a pod becomes Running, a `CREATED` event is queued
3. When a pod is deleted, a `DELETED` event is queued
4. Worker goroutines process events and POST deployment records to the
   API
5. Failed requests are automatically retried with exponential backoff

## Command Line Options

| Flag            | Description                          | Default                                    |
|-----------------|--------------------------------------|--------------------------------------------|
| `-kubeconfig`   | Path to kubeconfig file              | Uses in-cluster config or `~/.kube/config` |
| `-namespace`    | Namespace to monitor (empty for all) | `""` (all namespaces)                      |
| `-workers`      | Number of worker goroutines          | `2`                                        |
| `-metrics-port` | Port number for Prometheus metrics   | 9090                                       |

## Environment Variables

| Variable               | Description               | Default                                              |
|------------------------|---------------------------|------------------------------------------------------|
| `ORG`                  | GitHub organization name  | (required)                                           |
| `BASE_URL`             | API base URL              | `api.github.com`                                     |
| `DN_TEMPLATE`          | Deployment name template  | `{{namespace}}/{{deploymentName}}/{{containerName}}` |
| `LOGICAL_ENVIRONMENT`  | Logical environment name  | (required)                                           |
| `PHYSICAL_ENVIRONMENT` | Physical environment name | `""`                                                 |
| `CLUSTER`              | Cluster name              | (required)                                           |
| `API_TOKEN`            | API authentication token  | `""`                                                 |

> [!NOTE]
> The provisioned API token must have `artifact-metadata: write` with
> access to all relevant GitHub repositories (i.e all GitHub
> repositories that produces container images that are loaded into the
> cluster.

### Template Variables

The `DN_TEMPLATE` supports the following placeholders:
- `{{namespace}}` - Pod namespace
- `{{deploymentName}}` - Name of the owning Deployment
- `{{containerName}}` - Container name

## Kubernetes Deployment

A complete deployment manifest is provided in `deploy/manifest.yaml`
which includes:

- **Namespace**: `deployment-tracker`
- **ServiceAccount**: Identity for the controller pod
- **ClusterRole**: Minimal permissions (`get`, `list`, `watch` on pods)
- **ClusterRoleBinding**: Binds the ServiceAccount to the ClusterRole
- **Deployment**: Runs the controller with security hardening

### Verify Deployment

```bash
# Check the deployment status
kubectl get deployment -n deployment-tracker

# Check the pod is running
kubectl get pods -n deployment-tracker

# Verify RBAC permissions
kubectl auth can-i list pods --as=system:serviceaccount:deployment-tracker:deployment-tracker
```

### Uninstall

```bash
kubectl delete -f deploy/manifest.yaml
```

## RBAC Permissions

The controller requires the following minimum permissions:

| API Group | Resource | Verbs |
|-----------|----------|-------|
| `""` (core) | `pods` | `get`, `list`, `watch` |

If you only need to monitor a single namespace, you can modify the manifest to use a `Role` and `RoleBinding` instead of `ClusterRole` and `ClusterRoleBinding` for more restricted permissions.

## Architecture

```
┌─────────────────┐     ┌─────────────────┐     ┌─────────────────┐
│  Kubernetes     │     │   Controller    │     │   GitHub API    │
│  API Server     │────▶│                 │────▶│                 │
│                 │     │  ┌───────────┐  │     │                 │
│  Pod Events     │     │  │ Informer  │  │     │                 │
│  - Add          │     │  └─────┬─────┘  │     │                 │
│  - Update       │     │        │        │     │                 │
│  - Delete       │     │  ┌─────▼─────┐  │     │                 │
│                 │     │  │ Workqueue │  │     │                 │
│                 │     │  └─────┬─────┘  │     │                 │
│                 │     │        │        │     │                 │
│                 │     │  ┌─────▼─────┐  │     │                 │
│                 │     │  │ Workers   │──┼────▶│                 │
│                 │     │  └───────────┘  │     │                 │
└─────────────────┘     └─────────────────┘     └─────────────────┘
```

## Metrics

The deployment tracker provides Prometheus metrics, exposed via `http`
at `:9090/metrics`.  The port can be configured with the
`-metrics-port` flag (`9090` is the default).

The metrics exposed beyond the default Prometheus metrics are:

* `deptracker_events_processed_ok`: the total number of successful
  events process from the k8s API server. The metric is tagged the
  event type (`CRETE`/`DELETED`).
* `deptracker_events_processed_failed`: the total number of failed
  events process from the k8s API server. The metric is tagged the
  event type (`CRETE`/`DELETED`).
* `deptracker_events_processed_timer`: the processing time for each
  event. The metrics is tagged with the status of the event processing
  (`ok`/`failed`).
* `deptracker_post_deployment_record_timer`: the duration of the
  outgoing HTTP POST to upload the deployment record.
* `deptracker_post_record_ok`: the number of successful deployment
  record uploads.
* `deptracker_post_record_soft_fail`: the number of recoverable failed
  attempts to upload the deployment record.
* `deptracker_post_record_hard_fail`: the number of failures to
  persist a record via the HTTP API (either a irrecoverable error or
  all retries are exhausted).
