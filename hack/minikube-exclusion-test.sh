#!/usr/bin/env bash
# Regression test for https://github.com/castai/castai-pdb-controller/issues/10
# Requires: Docker (running), minikube, kubectl, helm
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing command: $1"; exit 1; }; }
need docker
need minikube
need kubectl
need helm

docker ps >/dev/null 2>&1 || {
	echo "Docker daemon is not reachable. Start Docker Desktop (or your Docker engine) and retry."
	exit 1
}

if ! minikube status >/dev/null 2>&1; then
	echo "Starting minikube..."
	minikube start
fi

echo "Building image into minikube Docker..."
eval "$(minikube docker-env)"
docker build -t castai/castai-pdb-controller:minikube-test .
eval "$(minikube docker-env -u)"

kubectl create namespace castai-agent --dry-run=client -o yaml | kubectl apply -f -

# Avoid stale Helm ownership on shared resource names (e.g. prior pdb-test release).
for rel in $(helm list -n castai-agent -q 2>/dev/null || true); do
	echo "Removing existing Helm release: $rel"
	helm uninstall "$rel" -n castai-agent --wait --timeout 3m || true
done
sleep 3

echo "Installing Helm chart (2 replicas, exclusions include istio-system)..."
helm upgrade --install castai-pdb ./helm/castai-pdb-controller \
	--namespace castai-agent \
	-f hack/minikube-exclusion-test.yaml \
	--wait --timeout 5m

kubectl apply -f hack/minikube-test-workloads.yaml

kubectl rollout status deployment/castai-pdb-controller -n castai-agent --timeout=180s
kubectl rollout status deployment/istiod -n istio-system --timeout=120s
kubectl rollout status deployment/web -n pdb-test-allowed --timeout=120s

echo "Waiting for informers and reconciliation..."
sleep 25

echo "--- PDBs in excluded namespace istio-system (must not include castai-*-pdb) ---"
kubectl get pdb -n istio-system -o wide 2>/dev/null || true

if kubectl get pdb -n istio-system -o name 2>/dev/null | grep -q '^poddisruptionbudget\.policy/castai-'; then
	echo "FAIL: castai PDB exists in excluded namespace istio-system"
	kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-pdb-controller --tail=100 --prefix=true || true
	exit 1
fi

echo "--- PDBs in pdb-test-allowed (expect castai-web-pdb) ---"
kubectl get pdb -n pdb-test-allowed -o wide
kubectl get pdb -n pdb-test-allowed -o name | grep -q 'castai-web-pdb' || {
	echo "FAIL: expected castai-web-pdb in pdb-test-allowed"
	exit 1
}

echo "Restarting controller deployment (reproduces startup / leader-election window)..."
kubectl rollout restart deployment/castai-pdb-controller -n castai-agent
kubectl rollout status deployment/castai-pdb-controller -n castai-agent --timeout=180s

echo "Waiting after restart..."
sleep 35

if kubectl get pdb -n istio-system -o name 2>/dev/null | grep -q '^poddisruptionbudget\.policy/castai-'; then
	echo "FAIL: after restart, castai PDB appeared in excluded namespace istio-system"
	kubectl logs -n castai-agent -l app.kubernetes.io/name=castai-pdb-controller --tail=100 --prefix=true || true
	exit 1
fi

echo "PASS: exclusions hold before and after controller restart (issue #10)."
