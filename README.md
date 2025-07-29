## CAST AI PDB Controller

A custom Kubernetes controller that automatically manages [PodDisruptionBudgets (PDBs)](https://kubernetes.io/docs/tasks/run-application/configure-pdb/) for Deployments and StatefulSets using flexible, annotation-driven configuration.  
This controller enables safe, automated disruption management with per-workload overrides and cluster-wide defaults.

---

## Features

- **Automatic PDB Creation/Update:**  
  Ensures every eligible Deployment/StatefulSet has a PDB, using defaults or per-workload overrides.

- **Annotation-Based Customization:**  
  Users can set `minAvailable`, `maxUnavailable`, or opt out of PDB management with annotations.

- **Dynamic Default Configuration:**  
    Cluster-wide default PDB values are set via a ConfigMap and can be changed at runtime without redeploying the controller.

- **Automatic Detection and Handling of Poor PDB Configurations:**  
  The controller detects overly restrictive or ineffective PodDisruptionBudgets (such as `minAvailable` equal to replicas, `minAvailable: 100%`, or `maxUnavailable: 0/0%`) that could block voluntary disruptions or cause operational issues.
  
  Through the `FixPoorPDBs` option in the `castai-pdb-controller-config` ConfigMap, you can choose whether the controller should only warn about these poor configurations (default) or automatically delete and recreate them with safe defaults. This ensures cluster upgrades, node drains, and scaling operations are not blocked by problematic PDBs.

- **Live Reconciliation:**  
  If annotations or ConfigMap values change, existing PDBs are updated automatically to reflect new requirements.

- **Bypass Support:**  
  Workloads can opt out of automatic PDB management at any time by adding a bypass annotation.

- **Garbage Collection:**  
  Orphaned PDBs are cleaned up when workloads are deleted or change state.

- **Leader Election:**  
  Supports safe, highly available operation in multi-replica controller deployments.

---

## Usage

### 1. **Default Behavior**

If no annotation is set, the controller uses the values from the `castai-pdb-controller-config` ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-pdb-controller-config
  namespace: castai-agent
data:
  minAvailable: "1"
  # maxUnavailable: "50%"  # Optional, use one or the other
  FixPoorPDBs: "true"  # Set to "true" to auto-fix poor PDBs, "false" to only warn
```

---

### 2. **Per-Workload Annotations**

Add annotations to your Deployment or StatefulSet to override the defaults:

#### **Set minAvailable**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: my-namespace
  annotations:
    workloads.cast.ai/pdb-minAvailable: "2"
spec:
  replicas: 3
  # ...
```

#### **Set maxUnavailable**

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: my-db
  namespace: my-namespace
  annotations:
    workloads.cast.ai/pdb-maxUnavailable: "25%"
spec:
  replicas: 4
  # ...
```

#### **Bypass PDB Creation**

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: no-pdb-app
  namespace: my-namespace
  annotations:
    workloads.cast.ai/bypass-default-pdb: "true"
spec:
  replicas: 5
  # ...
```

---

## Annotation Reference

| Annotation                                      | Description                                                      | Example Value      |
|-------------------------------------------------|------------------------------------------------------------------|--------------------|
| `workloads.cast.ai/pdb-minAvailable`            | Minimum pods that must be available (int or percent, one only)   | `"2"`, `"50%"`     |
| `workloads.cast.ai/pdb-maxUnavailable`          | Maximum pods that can be unavailable (int or percent, one only)  | `"1"`, `"25%"`     |
| `workloads.cast.ai/bypass-default-pdb`          | Opt out of automatic PDB management                              | `"true"`           |

---

## How It Works

- **On workload creation or update:**  
  The controller checks for annotations and creates/updates a PDB accordingly.
- **On annotation or ConfigMap change:**  
  The controller reconciles and updates existing PDBs to match new settings.
- **On workload deletion or bypass:**  
  The controller deletes the associated PDB.
- **On ConfigMap update:**  
  All workloads using the default config are updated to the new values.

---

## Requirements

- Kubernetes 1.21+
- Permissions to manage Deployments, StatefulSets, PDBs, and ConfigMaps in your cluster.
- RBAC rules that allow listing namespaces and managing PDBs at the cluster scope.

---

## Example: Full Deployment with Controller

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: my-namespace
  annotations:
    workloads.cast.ai/pdb-minAvailable: "2"
spec:
  replicas: 3
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
        - name: my-app
          image: my-app:latest
```

---

## Troubleshooting

- **Duplicate logs:**  
  Usually caused by log collector configuration, not the controller itself.
- **No PDB created:**  
  Ensure your workload has at least 2 replicas and is not opted out with the bypass annotation.
- **RBAC errors:**  
  Make sure your controller has permissions to list namespaces and manage PDBs.

---

For advanced usage, deployment via Helm, or troubleshooting, see the [controller source code](./main.go) and your cluster’s RBAC configuration.

If you decide to remove the castai-pdb-controller from your cluster, you need to run the following clean-up command if you'd like all custom-created PDBs to also be deleted.

```yaml
kubectl get poddisruptionbudget --all-namespaces -o custom-columns="NAMESPACE:.metadata.namespace,NAME:.metadata.name" \
  | awk '$2 ~ /^castai-.*-pdb$/ {print "kubectl delete poddisruptionbudget -n " $1 " " $2}' \
  | sh
```