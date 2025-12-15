# Deployment Tracker

A Kubernetes controller that monitors pod lifecycles and uploads
deployment records to GitHub's artifact metadata API.

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

| Flag          | Description                          | Default                                    |
|---------------|--------------------------------------|--------------------------------------------|
| `-kubeconfig` | Path to kubeconfig file              | Uses in-cluster config or `~/.kube/config` |
| `-namespace`  | Namespace to monitor (empty for all) | `""` (all namespaces)                      |
| `-workers`    | Number of worker goroutines          | `2`                                        |

## Environment Variables

| Variable               | Description               | Default                                              |
|------------------------|---------------------------|------------------------------------------------------|
| `ORG`                  | GitHub organization name  | (required)                                           |
| `BASE_URL`             | API base URL              | `api.github.com`                                     |
| `DN_TEMPLATE`          | Deployment name template  | `{{namespace}}/{{deploymentName}}/{{containerName}}` |
| `LOGICAL_ENVIRONMENT`  | Logical environment name  | `""`                                                 |
| `PHYSICAL_ENVIRONMENT` | Physical environment name | `""`                                                 |
| `CLUSTER`              | Cluster name              | `""`                                                 |
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

## Output Format

```
[2024-01-15T10:30:00Z] OK CREATED name=nginx deployment_name=default/nginx/nginx digest=sha256:abc123... status=deployed
[2024-01-15T10:30:10Z] OK DELETED name=nginx deployment_name=default/nginx/nginx digest=sha256:abc123... status=decommissioned
[2024-01-15T10:30:15Z] FAILED CREATED name=myapp deployment_name=default/myapp/app error=connection refused
```

## Kubernetes Deployment

A complete deployment manifest is provided in `deploy/manifest.yaml`
which includes:

- **Namespace**: `deployment-tracker`
- **ServiceAccount**: Identity for the controller pod
- **ClusterRole**: Minimal permissions (`get`, `list`, `watch` on pods)
- **ClusterRoleBinding**: Binds the ServiceAccount to the ClusterRole
- **Deployment**: Runs the controller with security hardening

### Deploy to Kubernetes

```
# Update the image in the manifest, then apply
kubectl apply -f deploy/manifest.yaml
```

### View Logs

```bash
# Follow logs from the controller
kubectl logs -f -n deployment-tracker deployment/deployment-tracker

# View recent logs
kubectl logs -n deployment-tracker deployment/deployment-tracker --tail=100
```

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
