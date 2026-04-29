## CAST AI PDB Controller

Kubernetes controller that creates and maintains [PodDisruptionBudgets](https://kubernetes.io/docs/tasks/run-application/configure-pdb/) for Deployments and StatefulSets. Defaults come from a ConfigMap; workloads can override or opt out with annotations.

### What it does

- Creates/updates PDBs for eligible workloads (e.g. ≥2 replicas), using ConfigMap defaults or annotations.
- Watches the `castai-pdb-controller-config` ConfigMap and workload changes; reconciles without redeploying the controller.
- Optional **poor PDB** detection (`FixPoorPDBs`: warn-only or auto-fix). **Exclusions** (regex on namespace, name, labels) skip PDB creation where matched.
- **Leader election** for HA replicas; garbage-collects orphaned `castai-*-pdb` objects.
- **Log level** via ConfigMap `logLevel` (Helm: `config.logLevel`).

---

## ConfigMap (`castai-pdb-controller-config`)

Typical keys (see `helm/castai-pdb-controller/values.yaml` for the full set):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: castai-pdb-controller-config
  namespace: castai-agent   # must match where the controller expects the CM
data:
  defaultMinAvailable: "1"   # or defaultMaxUnavailable — use one, not both
  FixPoorPDBs: "false"
  logLevel: "info"
  logInterval: "15m"
  pdbScanInterval: "2m"
  garbageCollectInterval: "2m"
  pdbDumpInterval: "5m"
  exclusions: |
    - namespaceRegex: "^kube-system$"
      nameRegex: ""
      labels: {}
    - namespaceRegex: ""
      nameRegex: ".*-temp$"
      labels: {}
```

**Exclusions:** each rule is independent (match any rule → no PDB). Inside one rule, namespace, name, and label filters are ANDed. Empty regex / `{}` labels means “don’t filter on that field.”

---

## Annotations

| Annotation | Purpose | Example |
|------------|---------|---------|
| `workloads.cast.ai/pdb-minAvailable` | Override min available | `"2"`, `"50%"` |
| `workloads.cast.ai/pdb-maxUnavailable` | Override max unavailable | `"1"`, `"25%"` |
| `workloads.cast.ai/bypass-default-pdb` | Skip automatic PDB | `"true"` |

Use only one of minAvailable / maxUnavailable on the workload (same as for PDBs).

---

## Log levels

Set `data.logLevel` on the ConfigMap (or env `CASTAI_PDB_CONTROLLER_LOG_LEVEL` only if `logLevel` is omitted from the ConfigMap).

| Value | Volume |
|-------|--------|
| `error` | Failures only (API errors, invalid rules, failed creates/deletes). |
| `warn` | Errors + warnings (bad intervals, selectors, poor PDB / multi-PDB warnings). Hides normal success lines. |
| `info` | **Default.** Normal operations; no `DEBUG:` trace. |
| `debug` | **Very verbose** — per-workload trace, reconciliation steps, exclusion dumps. Use short-lived for troubleshooting. |

Aliases: `d`, `i`, `w`, `e`, `warning`, `fatal`. Unknown values → `info` plus one warning.

Order: `debug` (chatty) → `info` → `warn` → `error` (quiet).

---

## Requirements

Kubernetes 1.21+, RBAC to list namespaces and manage PDBs (and related resources) as required by your install.

---

## Helm

Chart: `helm/castai-pdb-controller/`. Install with your registry/image tag and namespace aligned with the controller’s expected ConfigMap namespace.

---

## Troubleshooting

- **No PDB:** fewer than two replicas, bypass annotation, or exclusion rule match.
- **RBAC errors:** grant list/watch/create/update/delete on PDBs and read access to workloads/namespaces as needed.
- **Duplicate logs:** often the collector or multiple replicas; only the leader emits application logs.

---

## Uninstall: remove castai PDBs

```bash
kubectl get poddisruptionbudget --all-namespaces -o custom-columns="NAMESPACE:.metadata.namespace,NAME:.metadata.name" \
  | awk '$2 ~ /^castai-.*-pdb$/ {print "kubectl delete poddisruptionbudget -n " $1 " " $2}' \
  | sh
```

Source: [`cmd/`](./cmd/).
