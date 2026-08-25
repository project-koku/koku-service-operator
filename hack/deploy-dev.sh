#!/usr/bin/env bash
# AllNamespaces CRD + RBAC bootstrap for local / lab clusters (CRC, Cluster Bot, …).
# Does NOT deploy the manager — use make run (laptop) or hack/deploy-incluster.sh.
#
# Usage: ./hack/deploy-dev.sh [namespace]
#
# Alias: ./hack/deploy-crc.sh still works and calls this script.
set -euo pipefail

NS="${1:-cost-onprem}"

echo "=== Dev deploy (AllNamespaces CRD + RBAC) ==="
echo "Namespace: $NS"
echo "Cluster:   $(oc whoami --show-server 2>/dev/null || echo unknown)"
echo ""

# Create namespace
oc get namespace "$NS" &>/dev/null || oc create namespace "$NS"

# Install CRDs via kustomize (only the resources listed in config/crd/kustomization.yaml).
# Do not `oc apply -f config/crd/bases/` — that directory is a generator output and
# may contain leftover files from prior API-group renames.
echo "[1/4] Installing CRDs..."
make install

# Apply RBAC needed by the operator (AllNamespaces).
echo "[2/4] Applying RBAC..."
oc apply -f config/rbac/role.yaml
oc apply -f config/rbac/cluster_access_role.yaml
# Leader-election Role is namespaced — apply into the operator NS and bind default SA.
oc apply -n "$NS" -f config/rbac/leader_election_role.yaml

# Namespaced kinds (Secrets, Jobs, CMSC, …) cluster-wide so a CMSC outside
# this NS is still reconcilable. Drop leftover OwnNamespace RoleBinding.
oc delete rolebinding koku-operator-dev -n "$NS" 2>/dev/null || true
oc delete clusterrolebinding koku-operator-dev 2>/dev/null || true
oc create clusterrolebinding koku-operator-dev \
  --clusterrole=manager-role \
  --serviceaccount="$NS:default" \
  2>/dev/null || echo "  (clusterrolebinding koku-operator-dev already exists)"

oc create rolebinding koku-operator-dev-leader-election \
  --role=leader-election-role \
  --serviceaccount="$NS:default" \
  -n "$NS" \
  2>/dev/null || echo "  (rolebinding koku-operator-dev-leader-election already exists)"

# Cluster-scoped + NooBaa admin Secret get.
oc create clusterrolebinding koku-operator-dev-cluster \
  --clusterrole=manager-cluster-role \
  --serviceaccount="$NS:default" \
  2>/dev/null || echo "  (clusterrolebinding koku-operator-dev-cluster already exists)"

# Grant anyuid SCC so bundled PostgreSQL/Valkey pods can run with their
# required UIDs (postgres=999, valkey=999). Skip if not on OpenShift.
echo "[3/4] Granting anyuid SCC (bundled DB/Cache mode)..."
oc adm policy add-scc-to-user anyuid -z default -n "$NS" 2>/dev/null || true

echo "[4/4] Ready."
echo ""
echo "Next — pick ONE run mode:"
echo ""
echo "  # Laptop / CRC only (BYOI *.svc hosts are NOT resolvable from a laptop):"
echo "  NAMESPACE=$NS make run"
echo "  # or: NAMESPACE=$NS go run ./cmd/main.go --dev --operator-image=\$IMG"
echo ""
echo "  # Cluster Bot / any remote OpenShift (in-cluster manager):"
echo "  IMG=<registry>/koku-service-operator:<tag> ./hack/deploy-incluster.sh $NS"
echo ""
echo "Then apply a sample CR, e.g.:"
echo "  oc apply -n $NS -f config/samples/byoi/app/costmanagementserviceconfig-smoke.yaml"
echo ""
echo "Watch status:"
echo "  oc get costmanagementserviceconfig -n $NS -w"
echo "  oc describe costmanagementserviceconfig -n $NS"
echo ""
echo "Day-one Cluster Bot path: docs/development/clusterbot.md"
