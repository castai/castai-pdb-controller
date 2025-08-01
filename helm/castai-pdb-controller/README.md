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
# Install with default settings (no default PDB configuration)
helm install castai-pdb-controller castai/castai-pdb-controller \
  -n castai-agent --create-namespace

# Install with minAvailable mode enabled
helm install castai-pdb-controller castai/castai-pdb-controller \
  --set config.defaultMinAvailable="1" \
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
| `config.defaultMinAvailable` | Default minAvailable for PDBs | `"1"` (automatic PDB creation) |
| `config.defaultMaxUnavailable` | Default maxUnavailable for PDBs | `null` (unset) |
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

The controller supports two PDB configuration modes. By default, `minAvailable: "1"` is enabled for automatic PDB creation.

#### MinAvailable Mode
```yaml
config:
  defaultMinAvailable: "1"  # Ensures at least 1 pod is always available
  # or
  defaultMinAvailable: "50%"  # Ensures at least 50% of pods are always available
```

#### MaxUnavailable Mode
```yaml
config:
  defaultMaxUnavailable: "1"  # Allows at most 1 pod to be unavailable
  # or
  defaultMaxUnavailable: "50%"  # Allows up to 50% of pods to be unavailable
```

#### Disabling Default PDB Configuration
```yaml
config:
  defaultMinAvailable: null  # or "" or omit entirely
  defaultMaxUnavailable: null  # or "" or omit entirely
```

**Note**: Use either `defaultMinAvailable` or `defaultMaxUnavailable`, not both. If both are set, `defaultMinAvailable` takes precedence. The template ensures only one value is present in the ConfigMap.

### Using Helm --set Flag

You can use Helm's `--set` flag to override PDB configuration:

```bash
# Set minAvailable mode (default behavior)
helm install castai-pdb-controller castai/castai-pdb-controller \
  --set config.defaultMinAvailable="1" \
  -n castai-agent --create-namespace

# Set maxUnavailable mode (explicitly unset minAvailable)
helm install castai-pdb-controller castai/castai-pdb-controller \
  --set config.defaultMaxUnavailable="50%" \
  --set config.defaultMinAvailable=null \
  -n castai-agent --create-namespace

# Use defaults (minAvailable: "1" - automatic PDB creation)
helm install castai-pdb-controller castai/castai-pdb-controller \
  -n castai-agent --create-namespace
```

**Important**: When switching from minAvailable to maxUnavailable mode, you must explicitly unset minAvailable using `=null`. When switching from maxUnavailable to minAvailable mode, the template automatically handles the switch. This is due to the template's priority logic where minAvailable takes precedence.

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
# Upgrade to a new version
helm upgrade castai-pdb-controller castai/castai-pdb-controller \
  -n castai-agent

## Uninstalling

### Quick Uninstall
```bash
# Uninstall the chart (removes deployment, service account, and configmap)
helm uninstall castai-pdb-controller -n castai-agent

# Remove RBAC resources
kubectl delete clusterrole castai-pdb-controller
kubectl delete clusterrolebinding castai-pdb-controller
```

### Complete Cleanup (Optional)
If you want to remove everything including the PDBs created by the controller:

```bash
# Remove PDBs created by the controller (by name pattern)
kubectl get poddisruptionbudget --all-namespaces -o custom-columns="NAMESPACE:.metadata.namespace,NAME:.metadata.name" \
  | awk '$2 ~ /^castai-.*-pdb$/ {print "kubectl delete poddisruptionbudget -n " $1 " " $2}' \
  | sh

# Alternative: Remove specific CAST AI PDBs
kubectl delete pdb castai-test-pdb-pdb -n castai-agent
kubectl delete pdb castai-myapp-pdb -n my-namespace

# Remove namespace (if no other CAST AI components)
kubectl delete namespace castai-agent
```

**Note**: PDBs created by the controller will continue to protect your workloads even after uninstallation. Only remove them if you no longer need this protection.

**Identifying CAST AI PDBs**: The controller creates PDBs with the naming pattern `castai-{workload-name}-pdb`. You can list all CAST AI PDBs with:
```bash
kubectl get pdb -A | grep "^castai-.*-pdb"
```

## Support

For issues and questions:
- GitHub Issues: https://github.com/castai/castai-pdb-controller/issues
- Documentation: https://github.com/castai/castai-pdb-controller 