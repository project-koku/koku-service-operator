#!/usr/bin/env bash
# Cluster Bot / lab day-one smoke: BYOI infra + Redpanda + app Secrets + smoke CR.
# Does NOT build or deploy the operator — prints the next in-cluster step.
#
# Requires: oc (deploy-dev.sh uses oc for RBAC / SCC). kubectl alone is not enough.
#
# Usage (from repo root):
#   ./hack/clusterbot-smoke.sh
#   NAMESPACE=cost-byoi INFRA_NAMESPACE=cost-byoi-infra ./hack/clusterbot-smoke.sh
#
# Next:
#   IMG=quay.io/<you>/koku-service-operator:<tag> ./hack/deploy-incluster.sh "$NAMESPACE"
#
# See docs/development/clusterbot.md
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

NAMESPACE="${NAMESPACE:-cost-byoi}"
CR_NAME="${CR_NAME:-cost-management}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-cost-byoi-infra}"
SMOKE_CR="${SMOKE_CR:-config/samples/byoi/app/costmanagementserviceconfig-smoke.yaml}"
SECRETS_YAML="${SECRETS_YAML:-config/samples/byoi/app/secrets.yaml}"

if ! command -v oc >/dev/null 2>&1; then
  echo "error: oc is required (deploy-dev.sh and OpenShift ingress/SCC steps)" >&2
  exit 1
fi
KUBECTL="${KUBECTL:-oc}"

need_ns() {
  local ns="$1"
  if ! "$KUBECTL" get ns "$ns" >/dev/null 2>&1; then
    "$KUBECTL" create ns "$ns"
  fi
}

echo "=== Cluster Bot smoke (Redpanda BYOI) ==="
echo "App NS:    $NAMESPACE"
echo "Infra NS:  $INFRA_NAMESPACE"
echo "CR:        $CR_NAME"
echo "Cluster:   $($KUBECTL config current-context 2>/dev/null || echo unknown)"
echo ""
echo "NOTE: Do not use laptop 'make run' against this BYOI — *.svc is not"
echo "resolvable from your machine. Use ./hack/deploy-incluster.sh after this."
echo ""

# ---------------------------------------------------------------------------
# CRDs + RBAC (so the CR can be applied even before the manager is up)
# ---------------------------------------------------------------------------
echo "[1/4] CRDs + AllNamespaces RBAC (deploy-dev.sh)..."
./hack/deploy-dev.sh "$NAMESPACE"

# ---------------------------------------------------------------------------
# Infra + Redpanda
# ---------------------------------------------------------------------------
echo "[2/4] Postgres / Valkey / MinIO + Redpanda in ${INFRA_NAMESPACE}..."
need_ns "$INFRA_NAMESPACE"
oc adm policy add-scc-to-user anyuid -z byoi-infra -n "$INFRA_NAMESPACE" 2>/dev/null || true

TMP_INFRA="$(mktemp -d)"
cleanup() { rm -rf "$TMP_INFRA"; }
trap cleanup EXIT
cp -a "$ROOT/config/samples/byoi/infra/." "$TMP_INFRA/"
find "$TMP_INFRA" -type f \( -name '*.yaml' -o -name '*.yml' \) \
  -exec sed -i.bak "s/cost-byoi-infra/${INFRA_NAMESPACE}/g" {} +
find "$TMP_INFRA" -name '*.bak' -delete
"$KUBECTL" apply -k "$TMP_INFRA"
# Redpanda is optional in kustomize — apply explicitly (also retarget namespace).
sed "s/cost-byoi-infra/${INFRA_NAMESPACE}/g" "$ROOT/config/samples/byoi/infra/kafka.yaml" \
  | "$KUBECTL" apply -f -

"$KUBECTL" -n "$INFRA_NAMESPACE" rollout status deploy/postgresql --timeout=300s
"$KUBECTL" -n "$INFRA_NAMESPACE" rollout status deploy/valkey --timeout=180s
"$KUBECTL" -n "$INFRA_NAMESPACE" rollout status deploy/minio --timeout=180s
"$KUBECTL" -n "$INFRA_NAMESPACE" rollout status deploy/kafka --timeout=180s
"$KUBECTL" -n "$INFRA_NAMESPACE" wait --for=condition=complete job/minio-init --timeout=180s

echo "  Infra Ready: postgresql, valkey, minio, kafka (Redpanda)"

# ---------------------------------------------------------------------------
# Secrets + smoke CR
# ---------------------------------------------------------------------------
echo "[3/4] App Secrets + smoke CR..."
need_ns "$NAMESPACE"
if [[ ! -f "$SECRETS_YAML" ]]; then
  echo "error: secrets file not found: $SECRETS_YAML" >&2
  exit 1
fi
TMP_SECRETS="$(mktemp)"
# Drop the leading Namespace manifest; retarget Secret namespaces (same as deploy-byoi.sh).
awk '
  BEGIN { skip=0 }
  /^kind: Namespace$/ { skip=1; next }
  skip && /^---$/ { skip=0; next }
  skip { next }
  { print }
' "$SECRETS_YAML" \
  | sed "s/namespace: cost-byoi$/namespace: ${NAMESPACE}/" >"$TMP_SECRETS"
"$KUBECTL" apply -f "$TMP_SECRETS"
rm -f "$TMP_SECRETS"

DOMAIN="$("$KUBECTL" get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)"
TMP_CR="$(mktemp)"
# Retarget namespace / metadata.name / infra hosts / clusterDomain without
# rewriting unrelated "name:" fields (e.g. bucket names).
awk -v ns="$NAMESPACE" -v cr="$CR_NAME" -v infra="$INFRA_NAMESPACE" -v domain="$DOMAIN" '
  BEGIN { in_meta=0 }
  /^metadata:/ { in_meta=1 }
  /^spec:/ { in_meta=0 }
  {
    gsub(/cost-byoi-infra/, infra)
    if (domain != "") gsub(/apps\.cluster\.example\.com/, domain)
  }
  in_meta && /^  namespace: cost-byoi$/ { print "  namespace: " ns; next }
  in_meta && /^  name: cost-management$/ { print "  name: " cr; next }
  { print }
' "$SMOKE_CR" >"$TMP_CR"
"$KUBECTL" apply -f "$TMP_CR"
rm -f "$TMP_CR"

echo "  CR applied: ${NAMESPACE}/${CR_NAME}"
echo "  Kafka bootstrap: kafka.${INFRA_NAMESPACE}.svc.cluster.local:9092 (Redpanda)"
if [[ -n "$DOMAIN" ]]; then
  echo "  clusterDomain: ${DOMAIN}"
fi

# ---------------------------------------------------------------------------
# Next step
# ---------------------------------------------------------------------------
echo ""
echo "[4/4] Next — run the operator IN-CLUSTER (not laptop go run):"
echo ""
echo "  export IMG=quay.io/<you>/koku-service-operator:clusterbot"
echo "  docker buildx build --platform linux/amd64 -t \"\$IMG\" --push ."
echo "  IMG=\"\$IMG\" ./hack/deploy-incluster.sh ${NAMESPACE}"
echo ""
echo "Then (smoke CR is already applied — do not re-apply the AMQ Streams sample):"
echo "  oc -n ${NAMESPACE} get cmsc ${CR_NAME} -w"
echo "  oc -n ${NAMESPACE} get cmsc ${CR_NAME} -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){\"\\n\"}{end}'"
echo ""
echo "Day-one success: SchemaUpToDate + Available=True (KokuAvailable)."
echo "Phase stays Progressing / UIReady=False without Keycloak — that is OK."
echo "Full UI path: docs/development/pre-prod-install.md"
echo "Checklist: docs/development/clusterbot.md"
