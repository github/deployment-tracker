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
- **Runtime risks**: Track runtime risks through annotations

## How It Works

1. The controller watches for pod events using a Kubernetes
   SharedInformer
2. When a pod becomes Running, a `CREATED` event is queued
3. When a pod is deleted, a `DELETED` event is queued
4. Worker goroutines process events and POST deployment records to the
   API
5. Failed requests are automatically retried with exponential backoff

## Authentication

Two modes of authentication are supported:

1. Using a [GitHub
   App](https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/about-creating-github-apps#building-a-github-app).
1. Using PAT

> [!NOTE] The provisioned API token or GitHub App must have
> `artifact-metadata: write` with access to all relevant GitHub
> repositories (i.e all GitHub repositories that produces container
> images that are loaded into the cluster).

## Command Line Options

| Flag                  | Description                                                   | Default                                    |
|-----------------------|---------------------------------------------------------------|--------------------------------------------|
| `-kubeconfig`         | Path to kubeconfig file                                       | Uses in-cluster config or `~/.kube/config` |
| `-namespace`          | Namespace to monitor (empty for all)                          | `""` (all namespaces)                      |
| `-exclude-namespaces` | Comma-separated list of namespaces to exclude (empty for all) | `""` (all namespaces)                      |
| `-workers`            | Number of worker goroutines                                   | `2`                                        |
| `-metrics-port`       | Port number for Prometheus metrics                            | 9090                                       |

> [!NOTE]
> The `-namespace` and `-exclude-namespaces` flags cannot be used together.

## Environment Variables

| Variable               | Description                                | Default                                              |
|------------------------|--------------------------------------------|------------------------------------------------------|
| `ORG`                  | GitHub organization name                   | (required)                                           |
| `BASE_URL`             | API base URL                               | `api.github.com`                                     |
| `DN_TEMPLATE`          | Deployment name template                   | `{{namespace}}/{{deploymentName}}/{{containerName}}` |
| `LOGICAL_ENVIRONMENT`  | Logical environment name                   | (required)                                           |
| `PHYSICAL_ENVIRONMENT` | Physical environment name                  | `""`                                                 |
| `CLUSTER`              | Cluster name                               | (required)                                           |
| `API_TOKEN`            | API authentication token                   | `""`                                                 |
| `GH_APP_ID`            | GitHub App ID                              | `""`                                                 |
| `GH_INSTALL_ID`        | GitHub App installation ID                 | `""`                                                 |
| `GH_APP_PRIV_KEY`      | Path to the private key for the GitHub app | `""`                                                 |

### Template Variables

The `DN_TEMPLATE` supports the following placeholders:
- `{{namespace}}` - Pod namespace
- `{{deploymentName}}` - Name of the owning Deployment
- `{{containerName}}` - Container name

## Annotations 
Runtime risks and custom tags can be added to deployment records using annotations. Annotations will be aggregated from the pod and its owner reference objects (e.g. Deployment, ReplicaSet) so they can be added at any level of the ownership hierarchy.

### Runtime Risks

Runtime risks are risks associated with the deployment of an artifact. These risks can be used to filter GitHub Advanced Security (GHAS) alerts and add context to alert prioritization. 

Add the annotation `metadata.github.com/runtime-risks`, with a comma-separated list of supported runtime risk values. Annotations are aggregated from the pod and its owner reference objects. 

Currently supported runtime risks can be found in the [Create Deployment Record API docs](https://docs.github.com/en/rest/orgs/artifact-metadata?apiVersion=2022-11-28#create-an-artifact-deployment-record). Invalid runtime risk values will be ignored.

### Custom Tags
You can add custom tags to your deployment records to help filter and organize them in GitHub.

Add annotations with the prefix `metadata.github.com/<key>`  (e.g. `metadata.github.com/team: payments`) to add a custom tag. Annotations are aggregated from the pod and its owner reference objects.

If a key is seen at multiple levels of the ownership hierarchy, the value from the lowest level (closest to the pod) will take precedence. For example, if a tag key is present on both the pod and its owning deployment, the value from the pod will be used.

Currently, a maximum of 5 custom tags are allowed per deployment record. Custom tags will be ignored after the limit is reached, meaning tags lower in the ownership hierarchy will be prioritized. Tag keys and values must be less than 100 characters in length. Invalid tags will be ignored.

## Kubernetes Deployment

A complete deployment manifest is provided in `deploy/manifest.yaml`
which includes:

- **Namespace**: `deployment-tracker`
- **ServiceAccount**: Identity for the controller pod
- **ClusterRole**: Minimal permissions (`get`, `list`, `watch` on pods; `get` on other supported objects)
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
  events processed from the k8s API server. The metric is tagged the
  event type (`CREATED`/`DELETED`).
* `deptracker_events_processed_failed`: the total number of failed
  events processed from the k8s API server. The metric is tagged the
  event type (`CREATED`/`DELETED`).
* `deptracker_events_processed_timer`: the processing time for each
  event. The metric is tagged with the status of the event processing
  (`ok`/`failed`).
* `deptracker_post_deployment_record_timer`: the duration of the
  outgoing HTTP POST to upload the deployment record.
* `deptracker_post_record_ok`: the number of successful deployment
  record uploads.
* `deptracker_post_record_soft_fail`: the number of recoverable failed
  attempts to upload the deployment record.
* `deptracker_post_record_hard_fail`: the number of failures to
  persist a record via the HTTP API (either an irrecoverable error or
  all retries are exhausted).
* `deptracker_post_record_client_error`: the number of client errors,
  these are never retried nor reprocessed.

## License

This project is licensed under the terms of the MIT open source
license. Please refer to the [LICENSE](./LICENSE.txt) for the full
terms.
