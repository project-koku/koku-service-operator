#!/usr/bin/env bash
# Deploy optional BYOI lab dependencies for the Cost Management service operator:
#   Kafka (AMQ Streams) → Postgres/Valkey/MinIO → Keycloak/RHBK → UI OAuth mirror.
#
# This does NOT install the operator or apply the CostManagementServiceConfig.
# See docs/development/pre-prod-install.md (Part B) and hack/deploy-incluster.sh.
#
# Usage (from operator repo root):
#   ./hack/deploy-byoi.sh
#   NAMESPACE=cost-gold CR_NAME=cost-onprem INFRA_NAMESPACE=cost-gold-infra ./hack/deploy-byoi.sh
#
# Skip steps:
#   SKIP_KAFKA=1 SKIP_INFRA=1 SKIP_KEYCLOAK=1 SKIP_OAUTH_MIRROR=1 ./hack/deploy-byoi.sh
#
# Keycloak uses this repo's scripts/deploy-rhbk.sh by default.
# Override with RHBK_SCRIPT or CHART_ROOT (chart copy) if needed.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

NAMESPACE="${NAMESPACE:-cost-byoi}"
CR_NAME="${CR_NAME:-cost-management}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-cost-byoi-infra}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
STORAGE_CLASS="${STORAGE_CLASS:-}"
LOG_LEVEL="${LOG_LEVEL:-INFO}"
CHART_ROOT="${CHART_ROOT:-${ROOT}/../cost-onprem-chart}"
RHBK_SCRIPT="${RHBK_SCRIPT:-${ROOT}/scripts/deploy-rhbk.sh}"

SKIP_KAFKA="${SKIP_KAFKA:-0}"
SKIP_INFRA="${SKIP_INFRA:-0}"
SKIP_KEYCLOAK="${SKIP_KEYCLOAK:-0}"
SKIP_OAUTH_MIRROR="${SKIP_OAUTH_MIRROR:-0}"

KUBECTL="${KUBECTL:-kubectl}"
if ! command -v "$KUBECTL" >/dev/null 2>&1; then
  if command -v oc >/dev/null 2>&1; then
    KUBECTL=oc
  else
    echo "error: kubectl or oc is required" >&2
    exit 1
  fi
fi

if [[ -n "${KUBE_CONTEXT:-}" ]]; then
  current="$("$KUBECTL" config current-context 2>/dev/null || true)"
  if [[ "$current" != "$KUBE_CONTEXT" ]]; then
    echo "error: current-context is '${current:-<unset>}', expected '${KUBE_CONTEXT}'" >&2
    exit 1
  fi
fi

if [[ -z "$STORAGE_CLASS" ]]; then
  STORAGE_CLASS="$("$KUBECTL" get sc -o jsonpath='{.items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")].metadata.name}' 2>/dev/null | awk '{print $1}')"
  STORAGE_CLASS="${STORAGE_CLASS:-gp3-csi}"
fi

DOMAIN="$("$KUBECTL" get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)"
UI_BASE_URL="${COST_MGMT_UI_BASE_URL:-}"
if [[ -z "$UI_BASE_URL" && -n "$DOMAIN" ]]; then
  UI_BASE_URL="https://${CR_NAME}-ui-${NAMESPACE}.${DOMAIN}"
fi

echo "=== BYOI lab dependencies ==="
echo "App / operator NS:  $NAMESPACE"
echo "CR name:            $CR_NAME"
echo "Infra NS:           $INFRA_NAMESPACE"
echo "Kafka NS:           $KAFKA_NAMESPACE"
echo "Keycloak NS:        $KEYCLOAK_NAMESPACE"
echo "RHBK script:        $RHBK_SCRIPT"
echo "StorageClass:       $STORAGE_CLASS"
echo "UI base URL:        ${UI_BASE_URL:-<unset>}"
if [[ -n "${KUBE_CONTEXT:-}" ]]; then
  echo "Context:            ${KUBE_CONTEXT} ($("$KUBECTL" config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo unknown))"
else
  echo "Cluster:            $($KUBECTL config current-context 2>/dev/null || echo unknown)"
fi
echo ""

need_ns() {
  local ns="$1"
  if ! "$KUBECTL" get ns "$ns" >/dev/null 2>&1; then
    "$KUBECTL" create ns "$ns"
  fi
}

TMP_INFRA=""
cleanup() {
  [[ -n "$TMP_INFRA" && -d "$TMP_INFRA" ]] && rm -rf "$TMP_INFRA"
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
# A1 — Kafka (AMQ Streams)
# -----------------------------------------------------------------------------
if [[ "$SKIP_KAFKA" != "1" ]]; then
  echo "[A1] Kafka (AMQ Streams) in namespace ${KAFKA_NAMESPACE}..."
  KUBE_CONTEXT="${KUBE_CONTEXT:-}" KUBECTL="$KUBECTL" \
    STORAGE_CLASS="$STORAGE_CLASS" LOG_LEVEL="$LOG_LEVEL" \
    KAFKA_NAMESPACE="$KAFKA_NAMESPACE" \
    ./config/samples/byoi/deploy-kafka.sh
else
  echo "[A1] Skipping Kafka (SKIP_KAFKA=1)"
fi

# -----------------------------------------------------------------------------
# A2 — Postgres, Valkey, MinIO
# -----------------------------------------------------------------------------
if [[ "$SKIP_INFRA" != "1" ]]; then
  echo "[A2] Postgres / Valkey / MinIO in ${INFRA_NAMESPACE}..."
  need_ns "$INFRA_NAMESPACE"

  # Official postgres/MinIO images need anyuid on OpenShift.
  if [[ "$KUBECTL" == "oc" ]] || [[ "$(basename "$KUBECTL")" == "oc" ]]; then
    "$KUBECTL" adm policy add-scc-to-user anyuid -z byoi-infra -n "$INFRA_NAMESPACE" 2>/dev/null || true
  fi

  TMP_INFRA="$(mktemp -d)"
  # kustomize forbids absolute resource paths — copy the overlay locally.
  cp -a "$ROOT/config/samples/byoi/infra/." "$TMP_INFRA/"
  # Retarget every hard-coded sample namespace.
  find "$TMP_INFRA" -type f \( -name '*.yaml' -o -name '*.yml' \) \
    -exec sed -i.bak "s/cost-byoi-infra/${INFRA_NAMESPACE}/g" {} +
  find "$TMP_INFRA" -name '*.bak' -delete
  "$KUBECTL" apply -k "$TMP_INFRA"

  "$KUBECTL" -n "$INFRA_NAMESPACE" rollout status deploy/postgresql --timeout=300s
  "$KUBECTL" -n "$INFRA_NAMESPACE" rollout status deploy/valkey --timeout=180s
  "$KUBECTL" -n "$INFRA_NAMESPACE" rollout status deploy/minio --timeout=180s
  "$KUBECTL" -n "$INFRA_NAMESPACE" wait --for=condition=complete job/minio-init --timeout=180s
else
  echo "[A2] Skipping infra (SKIP_INFRA=1)"
fi

# -----------------------------------------------------------------------------
# A3 — Keycloak / RHBK (external chart script)
# -----------------------------------------------------------------------------
if [[ "$SKIP_KEYCLOAK" != "1" ]]; then
  echo "[A3] Keycloak / RHBK..."
  if [[ ! -x "$RHBK_SCRIPT" ]]; then
    echo "error: RHBK script not found or not executable: $RHBK_SCRIPT" >&2
    echo "  Set RHBK_SCRIPT, or SKIP_KEYCLOAK=1 if Keycloak already exists." >&2
    exit 1
  fi
  (
    cd "$(dirname "$RHBK_SCRIPT")"
    export RHBK_NAMESPACE="$KEYCLOAK_NAMESPACE"
    export COST_MGMT_NAMESPACE="$NAMESPACE"
    export COST_MGMT_RELEASE_NAME="$CR_NAME"
    export STORAGE_CLASS="$STORAGE_CLASS"
    export LOG_LEVEL="$LOG_LEVEL"
    export KUBECTL
    export KUBE_CONTEXT="${KUBE_CONTEXT:-}"
    if [[ -n "$UI_BASE_URL" ]]; then
      export COST_MGMT_UI_BASE_URL="$UI_BASE_URL"
    fi
    ./deploy-rhbk.sh
  )
else
  echo "[A3] Skipping Keycloak (SKIP_KEYCLOAK=1)"
fi

# -----------------------------------------------------------------------------
# A4 — App namespace + mirror UI OAuth client Secret
# -----------------------------------------------------------------------------
if [[ "$SKIP_OAUTH_MIRROR" != "1" ]]; then
  echo "[A4] Mirror UI OAuth client Secret into ${NAMESPACE}..."
  need_ns "$NAMESPACE"
  KUBE_CONTEXT="${KUBE_CONTEXT:-}" KUBECTL="$KUBECTL" \
    NAMESPACE="$NAMESPACE" CR_NAME="$CR_NAME" \
    KEYCLOAK_NAMESPACE="$KEYCLOAK_NAMESPACE" \
    ./config/samples/byoi/mirror-ui-oauth-secret.sh --force
else
  echo "[A4] Skipping OAuth mirror (SKIP_OAUTH_MIRROR=1)"
  need_ns "$NAMESPACE"
fi

# -----------------------------------------------------------------------------
# App Secrets (always ensure they exist in NAMESPACE — needed before CR)
# -----------------------------------------------------------------------------
echo "[A4b] App Secrets in ${NAMESPACE}..."
TMP_SECRETS="$(mktemp)"
# Drop the leading Namespace manifest; retarget Secret namespaces.
awk '
  BEGIN { skip=0 }
  /^kind: Namespace$/ { skip=1; next }
  skip && /^---$/ { skip=0; next }
  skip { next }
  { print }
' "$ROOT/config/samples/byoi/app/secrets.yaml" \
  | sed "s/namespace: cost-byoi$/namespace: ${NAMESPACE}/" >"$TMP_SECRETS"
"$KUBECTL" apply -f "$TMP_SECRETS"
rm -f "$TMP_SECRETS"

BOOTSTRAP="cost-onprem-kafka-kafka-bootstrap.${KAFKA_NAMESPACE}.svc.cluster.local:9092"
# Prefer an explicit env var from this shell. Only use deploy-kafka's
# /tmp/kafka-bootstrap-servers.env when it mentions the current KAFKA_NAMESPACE
# (avoids printing a stale bootstrap from a previous cluster).
if [[ -n "${KAFKA_BOOTSTRAP_SERVERS:-}" ]]; then
  BOOTSTRAP="$KAFKA_BOOTSTRAP_SERVERS"
elif [[ -f /tmp/kafka-bootstrap-servers.env ]]; then
  # shellcheck disable=SC1091
  # shellcheck disable=SC1090
  source /tmp/kafka-bootstrap-servers.env 2>/dev/null || true
  if [[ -n "${KAFKA_BOOTSTRAP_SERVERS:-}" && "${KAFKA_BOOTSTRAP_SERVERS}" == *"${KAFKA_NAMESPACE}"* ]]; then
    BOOTSTRAP="$KAFKA_BOOTSTRAP_SERVERS"
  elif [[ -n "${KAFKA_BOOTSTRAP_SERVERS:-}" ]]; then
    echo "warning: ignoring stale /tmp/kafka-bootstrap-servers.env (not for ${KAFKA_NAMESPACE}); using ${BOOTSTRAP}" >&2
    unset KAFKA_BOOTSTRAP_SERVERS
  fi
fi

echo ""
echo "=== BYOI dependencies ready ==="
echo "Postgres:  postgresql.${INFRA_NAMESPACE}.svc.cluster.local:5432"
echo "Valkey:    valkey.${INFRA_NAMESPACE}.svc.cluster.local:6379"
echo "MinIO:     minio.${INFRA_NAMESPACE}.svc.cluster.local:9000"
echo "Kafka:     ${BOOTSTRAP}"
echo "Keycloak:  http://keycloak-service.${KEYCLOAK_NAMESPACE}.svc.cluster.local:8080"
echo "OAuth Secret: ${NAMESPACE}/${CR_NAME}-ui-oauth-client"
if [[ -n "$DOMAIN" ]]; then
  echo "Cluster domain: ${DOMAIN}"
  echo "Expected UI URL: ${UI_BASE_URL}"
fi
echo ""
echo "Next (Part B): build/push an amd64 operator image, then:"
echo "  IMG=<image> ./hack/deploy-incluster.sh ${NAMESPACE}"
echo "Render/apply a CR with matching hosts (see docs/development/pre-prod-install.md)."
