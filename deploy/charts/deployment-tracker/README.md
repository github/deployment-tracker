# deployment-tracker Helm Chart

A Helm chart for deploying the deployment-tracker Kubernetes controller, which
monitors pod lifecycles and uploads deployment records to GitHub's artifact
metadata API.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.8+
- A GitHub organization with
  [Artifact Attestations](https://docs.github.com/en/actions/concepts/security/artifact-attestations)
  enabled
- An API token or GitHub App for authentication

## Installation

### Install from OCI registry

```bash
helm install deployment-tracker \
  oci://ghcr.io/github/charts/deployment-tracker \
  --namespace deployment-tracker \
  --create-namespace \
  --set config.org=my-org \
  --set config.logicalEnvironment=production \
  --set config.cluster=my-cluster \
  --set auth.apiTokenSecret=my-api-token-secret
```

### Install from local source

```bash
helm install deployment-tracker ./deploy/charts/deployment-tracker \
  --namespace deployment-tracker \
  --create-namespace \
  -f my-values.yaml
```

## Required Values

The following values **must** be set for the controller to function:

| Value | Description |
|-------|-------------|
| `config.org` | GitHub organization name |
| `config.logicalEnvironment` | Logical environment name (e.g., `production`, `staging`) |
| `config.cluster` | Kubernetes cluster name |

At least one authentication method must be configured (see
[Authentication](#authentication)).

## Configuration

### Controller Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `config.org` | GitHub organization name | `""` (required) |
| `config.logicalEnvironment` | Logical environment name | `""` (required) |
| `config.cluster` | Cluster name | `""` (required) |
| `config.physicalEnvironment` | Physical environment name | `""` |
| `config.baseUrl` | API base URL | `api.github.com` |
| `config.dnTemplate` | Deployment name template | `{{namespace}}/{{deploymentName}}/{{containerName}}` |
| `config.namespace` | Namespace to monitor (empty for all) | `""` |
| `config.excludeNamespaces` | Namespaces to exclude (comma-separated) | `""` |
| `config.workers` | Number of worker goroutines | `2` |
| `config.metricsPort` | Port for Prometheus metrics | `9090` |

### Image Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Container image repository | `ghcr.io/github/deployment-tracker` |
| `image.tag` | Container image tag | Chart `appVersion` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `imagePullSecrets` | Image pull secrets | `[]` |

### Authentication

The controller supports two authentication methods: API token and GitHub App.

#### API Token

```yaml
# Option 1: Inline token (not recommended for production)
auth:
  apiToken: "ghp_xxxxxxxxxxxx"

# Option 2: Reference to an existing Secret (recommended)
auth:
  apiTokenSecret: "my-api-token-secret"
```

When using `apiTokenSecret`, the Secret must have a key named `token`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-api-token-secret
type: Opaque
stringData:
  token: "ghp_xxxxxxxxxxxx"
```

#### GitHub App

```yaml
# Option 1: Inline private key
auth:
  githubApp:
    appId: "12345"
    installId: "67890"
    privateKey: "<base64-encoded-private-key>"

# Option 2: Reference to an existing Secret (recommended)
auth:
  githubApp:
    appId: "12345"
    installId: "67890"
    privateKeySecret: "my-github-app-secret"
```

When using `privateKeySecret`, the Secret must have a key named `private-key`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-github-app-secret
type: Opaque
stringData:
  private-key: "<base64-encoded-private-key>"
```

### Resources and Security

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of controller replicas | `1` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `1024Mi` |
| `resources.limits.cpu` | CPU limit | `200m` |
| `resources.limits.memory` | Memory limit | `2048Mi` |
| `securityContext` | Container security context | Hardened (see values.yaml) |
| `readinessProbe` | Readiness probe configuration | HTTP GET /metrics:9090 |
| `lifecycle` | Lifecycle hooks | preStop sleep 5s |

### Service and Networking

| Parameter | Description | Default |
|-----------|-------------|---------|
| `service.enabled` | Create a Service for metrics | `true` |
| `service.type` | Service type | `ClusterIP` |
| `service.port` | Service port | `9090` |
| `nodeSelector` | Node selector | `{}` |
| `tolerations` | Tolerations | `[]` |
| `affinity` | Affinity rules | `{}` |

### Service Account

| Parameter | Description | Default |
|-----------|-------------|---------|
| `serviceAccount.create` | Create a ServiceAccount | `true` |
| `serviceAccount.name` | ServiceAccount name | Generated from fullname |
| `serviceAccount.annotations` | ServiceAccount annotations | `{}` |

## Uninstalling

```bash
helm uninstall deployment-tracker -n deployment-tracker
```
