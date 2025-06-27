# CAST AI PDB Controller Helm Chart

A Helm chart for deploying the CAST AI Pod Disruption Budget (PDB) Controller to Kubernetes clusters.

## Overview

The CAST AI PDB Controller automatically creates, updates, and manages PodDisruptionBudgets for Deployments and StatefulSets in your Kubernetes cluster. It ensures high availability during cluster maintenance and node disruptions by:

- Automatically creating PDBs for Deployments and StatefulSets
- Monitoring and fixing poor PDB configurations
- Supporting annotation-driven PDB customization
- Providing configurable intervals for scanning and maintenance

## Prerequisites

- Kubernetes 1.21+
- Helm 3.0+
- Cluster admin permissions (for RBAC resources)

## Installation

### Quick Start

```bash
# Add the CAST AI Helm repository
helm repo add castai https://castai.github.io/castai-pdb-controller

# Update the repository
helm repo update

# Install the chart into the castai-agent namespace
helm install castai-pdb-controller castai/castai-pdb-controller \
  -n castai-agent --create-namespace
```

### Installation Options

#### Basic Installation
```bash
# Install with default settings
helm install castai-pdb-controller castai/castai-pdb-controller \
  -n castai-agent --create-namespace
```

#### Custom Configuration
```bash
# Install with custom PDB settings
helm install castai-pdb-controller castai/castai-pdb-controller \
  --set config.defaultMinAvailable="2" \
  --set config.FixPoorPDBs=true \
  --set config.logInterval="10m" \
  -n castai-agent --create-namespace
```

#### Production Installation
```bash
# Install with production-ready settings
helm install castai-pdb-controller castai/castai-pdb-controller \
  --set replicaCount=3 \
  --set resources.limits.cpu=1000m \
  --set resources.limits.memory=1Gi \
  --set resources.requests.cpu=200m \
  --set resources.requests.memory=256Mi \
  --set config.defaultMinAvailable="1" \
  --set config.FixPoorPDBs=true \
  --set config.pdbScanInterval="1m" \
  --set config.garbageCollectInterval="5m" \
  -n castai-agent --create-namespace
```

## Configuration

### Values Reference

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of controller replicas | `2` |
| `image.repository` | Container image repository | `castai/castai-pdb-controller` |
| `image.tag` | Container image tag | `"latest"` |
| `image.pullPolicy` | Container image pull policy | `IfNotPresent` |
| `nameOverride` | Override the chart name | `""` |
| `fullnameOverride` | Override the full app name | `""` |
| `serviceAccount.create` | Create a new service account | `true` |
| `serviceAccount.name` | Service account name | `"castai-pdb-controller"` |
| `serviceAccount.annotations` | Service account annotations | `{}` |
| `rbac.create` | Create RBAC resources | `true` |
| `config.defaultMinAvailable` | Default minAvailable for PDBs | `"1"` |
| `config.defaultMaxUnavailable` | Default maxUnavailable for PDBs | `""` (unset) |
| `config.FixPoorPDBs` | Automatically fix poor PDB configurations | `"false"` |
| `config.logInterval` | Log interval for repeated messages | `"15m"` |
| `config.pdbScanInterval` | PDB scan interval | `"2m"` |
| `config.garbageCollectInterval` | Garbage collection interval | `"2m"` |
| `config.pdbDumpInterval` | PDB dump interval | `"5m"` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `512Mi` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `128Mi` |
| `securityContext.container` | Container security context | See values.yaml |
| `securityContext.pod` | Pod security context | See values.yaml |
| `pod.annotations` | Pod annotations | `{}` |
| `pod.nodeSelector` | Node selector | `{}` |
| `pod.tolerations` | Tolerations | `[]` |
| `pod.affinity` | Affinity rules | `{}` |
| `strategy.type` | Deployment strategy | `RollingUpdate` |
| `strategy.rollingUpdate` | Rolling update config | See values.yaml |

### PDB Configuration

The controller supports two PDB configuration modes:

#### MinAvailable Mode (Default)
```yaml
config:
  defaultMinAvailable: "1"  # Ensures at least 1 pod is always available
```

#### MaxUnavailable Mode
```yaml
config:
  defaultMaxUnavailable: "50%"  # Allows up to 50% of pods to be unavailable
```

**Note**: Use either `defaultMinAvailable` or `defaultMaxUnavailable`, not both.

### Advanced Configuration Examples

#### Custom Resource Limits
```yaml
resources:
  limits:
    cpu: 1000m
    memory: 1Gi
  requests:
    cpu: 200m
    memory: 256Mi
```

#### Custom Security Context
```yaml
securityContext:
  container:
    allowPrivilegeEscalation: false
    runAsNonRoot: true
    runAsUser: 1000
    capabilities:
      drop: ["ALL"]
  pod:
    fsGroup: 1000
    runAsNonRoot: true
    runAsUser: 1000
```

#### Node Affinity and Tolerations
```yaml
pod:
  nodeSelector:
    node-role.kubernetes.io/worker: "true"
  tolerations:
    - key: "node-role.kubernetes.io/control-plane"
      operator: "Exists"
      effect: "NoSchedule"
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: node-role.kubernetes.io/worker
            operator: In
            values:
            - "true"
```

#### Custom Deployment Strategy
```yaml
strategy:
  type: RollingUpdate
  rollingUpdate:
    maxSurge: 50%
    maxUnavailable: 0
```

## Usage

### Workload Annotations

The controller supports annotation-driven PDB configuration:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  annotations:
    workloads.cast.ai/pdb-minAvailable: "2"
    workloads.cast.ai/pdb-maxUnavailable: "25%"
    workloads.cast.ai/bypass-default-pdb: "true"  # Skip PDB creation
spec:
  replicas: 4
  # ... rest of deployment spec
```

### Controller Behavior

1. **Automatic PDB Creation**: Creates PDBs for Deployments and StatefulSets without existing PDBs
2. **Poor Configuration Detection**: Identifies and optionally fixes PDBs that don't match workload replicas
3. **Garbage Collection**: Removes orphaned PDBs when workloads are deleted
4. **Leader Election**: Ensures only one controller instance is active at a time

## Monitoring

### Check Controller Status

```bash
# Check pods
kubectl get pods -n castai-agent -l app.kubernetes.io/name=castai-pdb-controller

# Check logs
kubectl logs deployment/castai-pdb-controller -n castai-agent

# Check created PDBs
kubectl get pdb -A | grep castai

# Check ConfigMap
kubectl get configmap castai-pdb-controller-config -n castai-agent -o yaml
```

### Check Controller Health

```bash
# Check deployment status
kubectl get deployment castai-pdb-controller -n castai-agent

# Check events
kubectl get events -n castai-agent --sort-by='.lastTimestamp'

# Check RBAC permissions
kubectl auth can-i create poddisruptionbudgets --as=system:serviceaccount:castai-agent:castai-pdb-controller

# Check leader election lease
kubectl get lease castai-pdb-controller-leader-election -n castai-agent
```

### Troubleshooting

```bash
# Check controller logs for errors
kubectl logs deployment/castai-pdb-controller -n castai-agent --tail=100

# Check if ConfigMap is loaded correctly
kubectl describe configmap castai-pdb-controller-config -n castai-agent

# Verify RBAC resources
kubectl get clusterrole castai-pdb-controller
kubectl get clusterrolebinding castai-pdb-controller
```

## Upgrading

```bash
# Update the repository
helm repo update

# Upgrade to a new version
helm upgrade castai-pdb-controller castai/castai-pdb-controller \
  -n castai-agent

# Upgrade with new values
helm upgrade castai-pdb-controller castai/castai-pdb-controller \
  --set image.tag="latest" \
  --set config.defaultMinAvailable="2" \
  --set config.FixPoorPDBs=true \
  -n castai-agent
```

## Uninstalling

```bash
# Uninstall the chart
helm uninstall castai-pdb-controller -n castai-agent

# Remove RBAC resources (if not managed by Helm)
kubectl delete clusterrole castai-pdb-controller
kubectl delete clusterrolebinding castai-pdb-controller

# Remove the namespace (optional)
kubectl delete namespace castai-agent
```

## Development

### Local Testing

```bash
# Package the chart
helm package helm/castai-pdb-controller

# Lint the chart
helm lint helm/castai-pdb-controller

# Template rendering test
helm template castai-pdb-controller helm/castai-pdb-controller \
  --set config.defaultMinAvailable="2" \
  --set config.FixPoorPDBs=true
```

## Support

For issues and questions:
- GitHub Issues: https://github.com/castai/castai-pdb-controller/issues
- Documentation: https://github.com/castai/castai-pdb-controller 