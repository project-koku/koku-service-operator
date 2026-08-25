#!/usr/bin/env bash
# Prow / Cluster Bot: BYOI + CMSC Ready + pytest against an already-installed
# operator. Does NOT install the operator (OLM `install` step or
# hack/deploy-incluster.sh must have already run). Does NOT call Helm
# (scripts/install-cmsc.sh is the chart installer — never use it here).
#
# Usage (from operator repo root, operator already installed):
#   NAMESPACE=cost-onprem CR_NAME=cost-onprem INFRA_NAMESPACE=cost-onprem-infra \
#     CHART_ROOT=/path/to/cost-onprem-chart ./hack/ci/e2e.sh
#
# Prow sets ARTIFACT_DIR. Locally, JUnit is still written under test/pytest/reports/.
# Artifact dumps are redacted and never include Secret/ConfigMap payloads.
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

NAMESPACE="${NAMESPACE:-cost-onprem}"
CR_NAME="${CR_NAME:-cost-onprem}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-cost-onprem-infra}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-koku-service-operator-system}"
CMSC_READY_TIMEOUT="${CMSC_READY_TIMEOUT:-45m}"
SKIP_PYTEST="${SKIP_PYTEST:-0}"
SAMPLE_CR="${SAMPLE_CR:-config/samples/byoi/app/costmanagementserviceconfig.yaml}"

KUBECTL="${KUBECTL:-}"
if [[ -z "$KUBECTL" ]]; then
  if command -v oc >/dev/null 2>&1; then
    KUBECTL=oc
  elif command -v kubectl >/dev/null 2>&1; then
    KUBECTL=kubectl
  else
    echo "error: oc or kubectl is required" >&2
    exit 1
  fi
fi

if ! command -v kubectl >/dev/null 2>&1 && command -v oc >/dev/null 2>&1; then
  mkdir -p /tmp/bin
  ln -sf "$(command -v oc)" /tmp/bin/kubectl
  export PATH="/tmp/bin:${PATH}"
fi

resolve_chart_root() {
  local script="scripts/deploy-rhbk.sh"
  local c
  local candidates=(
    "${CHART_ROOT:-}"
    "${HOME}/go/src/github.com/insights-onprem/cost-onprem-chart"
    "${GOPATH:-}/src/github.com/insights-onprem/cost-onprem-chart"
    "/go/src/github.com/insights-onprem/cost-onprem-chart"
    "/home/prow/go/src/github.com/insights-onprem/cost-onprem-chart"
    "${ROOT}/../cost-onprem-chart"
  )
  for c in "${candidates[@]}"; do
    [[ -n "$c" && -x "${c}/${script}" ]] && { echo "$c"; return 0; }
  done
  return 1
}

# ci-operator tests cannot declare extra_refs. Clone the chart for RHBK when
# it is not already on disk (sibling checkout or a Prow clonerefs path).
ensure_chart_root() {
  local found
  found="$(resolve_chart_root || true)"
  if [[ -n "$found" ]]; then
    echo "$found"
    return 0
  fi
  if [[ "${SKIP_KEYCLOAK:-0}" == "1" ]]; then
    return 0
  fi
  if ! command -v git >/dev/null 2>&1; then
    echo "error: git is required to clone insights-onprem/cost-onprem-chart for RHBK" >&2
    echo "  Set CHART_ROOT, SKIP_KEYCLOAK=1, or install git." >&2
    return 1
  fi
  local dest="${CHART_CLONE_DIR:-/tmp/cost-onprem-chart}"
  echo "CHART_ROOT unset; cloning insights-onprem/cost-onprem-chart@${CHART_REF:-main} → ${dest}" >&2
  rm -rf "$dest"
  git clone --depth 1 --branch "${CHART_REF:-main}" \
    https://github.com/insights-onprem/cost-onprem-chart.git "$dest"
  echo "$dest"
}

# Redact credential-shaped strings on stdin. Used for anything that may land in
# ARTIFACT_DIR or the Prow build log (CMSC status, events, operator logs, JUnit).
redact_filter() {
  if ! command -v python3 >/dev/null 2>&1; then
    cat
    return 0
  fi
  python3 -c "$(cat <<'PY'
import re
import sys

def redact(text: str) -> str:
    text = re.sub(
        r"eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}",
        "<redacted-jwt>",
        text,
    )
    text = re.sub(r"AKIA[0-9A-Z]{16}", "<redacted-aws-access-key>", text)
    text = re.sub(
        r"(?i)(bearer\s+)[A-Za-z0-9._\-+/=]+",
        r"\1<redacted-token>",
        text,
    )
    text = re.sub(
        r"(://[^:/?#\s]+):([^@/?#\s]+)@",
        r"\1:<redacted>@",
        text,
    )
    text = re.sub(
        r'(?i)\b(password|passwd|client_secret|secret_key|access_key|access-key|secret-key|access_token|refresh_token)\b(\s*[=:]\s*)([^\s&"\'<>]+)',
        r"\1\2<redacted>",
        text,
    )
    text = re.sub(
        r'(?i)("(?:password|passwd|client_secret|secret_key|access_key|access-key|secret-key|access_token|refresh_token)"\s*:\s*")[^"]*(")',
        r"\1<redacted>\2",
        text,
    )
    return text

sys.stdout.write(redact(sys.stdin.read()))
PY
)"
}

# kubectl -o json → drop managedFields / last-applied-configuration, then redact.
sanitize_k8s_json() {
  if ! command -v python3 >/dev/null 2>&1; then
    redact_filter
    return 0
  fi
  python3 -c "$(cat <<'PY'
import json
import sys

def strip_meta(obj):
    if not isinstance(obj, dict):
        return obj
    md = obj.get("metadata")
    if isinstance(md, dict):
        md.pop("managedFields", None)
        anns = md.get("annotations")
        if isinstance(anns, dict):
            anns.pop("kubectl.kubernetes.io/last-applied-configuration", None)
            if not anns:
                md.pop("annotations", None)
    return obj

raw = sys.stdin.read()
if not raw.strip():
    sys.exit(0)
obj = json.loads(raw)
if isinstance(obj, dict) and obj.get("kind") == "List" and isinstance(obj.get("items"), list):
    obj["items"] = [strip_meta(item) for item in obj["items"]]
else:
    obj = strip_meta(obj)
json.dump(obj, sys.stdout, indent=2)
sys.stdout.write("\n")
PY
)" | redact_filter
}

dump() {
  local dest="${ARTIFACT_DIR:-${ROOT}/test/pytest/reports}"
  mkdir -p "$dest"
  echo "=== dumping artifacts to ${dest} ==="
  "$KUBECTL" -n "${OPERATOR_NAMESPACE}" logs deploy/koku-service-operator-controller-manager \
    2>/dev/null | redact_filter >"${dest}/operator.log" || true
  # Test CR only (never -A). JSON so we can strip managedFields / last-applied.
  "$KUBECTL" -n "${NAMESPACE}" get cmsc "${CR_NAME}" -o json \
    2>/dev/null | sanitize_k8s_json >"${dest}/cmsc-${CR_NAME}.json" || true
  "$KUBECTL" -n "${NAMESPACE}" get cmsc "${CR_NAME}" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}' \
    2>/dev/null | redact_filter >"${dest}/cmsc-conditions.txt" || true
  "$KUBECTL" -n "${NAMESPACE}" get events --sort-by=.lastTimestamp \
    2>/dev/null | redact_filter >"${dest}/events-${NAMESPACE}.txt" || true
  "$KUBECTL" -n "${OPERATOR_NAMESPACE}" get events --sort-by=.lastTimestamp \
    2>/dev/null | redact_filter >"${dest}/events-operator.txt" || true
  # Table output only. Do not include secret (or -o yaml): payloads must not
  # reach ARTIFACT_DIR. ConfigMap names/key-counts are ok; values are not.
  "$KUBECTL" -n "${NAMESPACE}" get deploy,po,svc,route,cm -o wide \
    >"${dest}/app-resources.txt" 2>/dev/null || true
}

copy_junit() {
  local src="${ROOT}/test/pytest/reports/junit.xml"
  local dest_dir="${ARTIFACT_DIR:-${ROOT}/test/pytest/reports}"
  mkdir -p "$dest_dir"
  if [[ -f "$src" ]]; then
    redact_filter <"$src" >"${dest_dir}/junit_e2e.xml"
    echo "JUnit: ${dest_dir}/junit_e2e.xml"
  else
    echo "warning: no pytest JUnit at ${src}" >&2
  fi
}

# Copy a Secret under a new name without kubectl apply. Client-side apply
# writes the full object (including data) into last-applied-configuration.
copy_secret() {
  local src="$1" dst="$2" ns="$3"
  local verb=create
  if ! command -v python3 >/dev/null 2>&1; then
    echo "error: python3 is required to copy Secret ${src} → ${dst}" >&2
    exit 1
  fi
  if "$KUBECTL" -n "$ns" get secret "$dst" >/dev/null 2>&1; then
    verb=replace
  fi
  "$KUBECTL" -n "$ns" get secret "$src" -o json | python3 -c '
import json, sys
name = sys.argv[1]
obj = json.load(sys.stdin)
md = obj.get("metadata") or {}
keep = {"apiVersion", "kind", "metadata", "type"}
if "data" in obj:
    keep.add("data")
if "stringData" in obj:
    keep.add("stringData")
if "immutable" in obj:
    keep.add("immutable")
for key in list(obj):
    if key not in keep:
        obj.pop(key, None)
obj["metadata"] = {
    "name": name,
    "namespace": md.get("namespace") or "",
    "labels": md.get("labels") or {},
}
json.dump(obj, sys.stdout)
' "$dst" | "$KUBECTL" "$verb" -f -
}

if ! "$KUBECTL" get crd costmanagementserviceconfigs.service.costmanagement.openshift.io >/dev/null 2>&1; then
  echo "error: CMSC CRD not found. Install the operator first" >&2
  echo "  (Prow e2e 'install' step, or IMG=… ./hack/deploy-incluster.sh ${NAMESPACE})." >&2
  exit 1
fi

CHART_ROOT="$(ensure_chart_root)"
export CHART_ROOT

echo "=== operator e2e (BYOI + CMSC + pytest) ==="
echo "App NS:       $NAMESPACE"
echo "CR name:      $CR_NAME"
echo "Infra NS:     $INFRA_NAMESPACE"
echo "Operator NS:  $OPERATOR_NAMESPACE"
echo "CHART_ROOT:   ${CHART_ROOT:-<unset>}"
echo "Cluster:      $($KUBECTL config current-context 2>/dev/null || echo unknown)"
echo ""

trap dump EXIT

"$KUBECTL" create namespace "${NAMESPACE}" --dry-run=client -o yaml | "$KUBECTL" apply -f -

echo "[1/4] BYOI dependencies..."
NAMESPACE="$NAMESPACE" CR_NAME="$CR_NAME" INFRA_NAMESPACE="$INFRA_NAMESPACE" \
  KAFKA_NAMESPACE="$KAFKA_NAMESPACE" KEYCLOAK_NAMESPACE="$KEYCLOAK_NAMESPACE" \
  CHART_ROOT="${CHART_ROOT:-}" \
  ./hack/deploy-byoi.sh

echo "[2/4] Pytest-compatible Secret names ({cr.name}-*)..."
copy_secret byoi-db-credentials "${CR_NAME}-db-credentials" "$NAMESPACE"
copy_secret byoi-cache-credentials "${CR_NAME}-cache-credentials" "$NAMESPACE"
copy_secret byoi-s3-credentials "${CR_NAME}-storage-credentials" "$NAMESPACE"

DOMAIN="$("$KUBECTL" get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)"
BOOTSTRAP="cost-onprem-kafka-kafka-bootstrap.${KAFKA_NAMESPACE}.svc.cluster.local:9092"
if [[ -n "${KAFKA_BOOTSTRAP_SERVERS:-}" ]]; then
  BOOTSTRAP="$KAFKA_BOOTSTRAP_SERVERS"
elif [[ -f /tmp/kafka-bootstrap-servers.env ]]; then
  # shellcheck disable=SC1091
  source /tmp/kafka-bootstrap-servers.env 2>/dev/null || true
  if [[ -n "${KAFKA_BOOTSTRAP_SERVERS:-}" && "${KAFKA_BOOTSTRAP_SERVERS}" == *"${KAFKA_NAMESPACE}"* ]]; then
    BOOTSTRAP="$KAFKA_BOOTSTRAP_SERVERS"
  fi
fi

echo "[3/4] Apply CMSC ${NAMESPACE}/${CR_NAME} (ros.enabled=false)..."
TMP_CR="$(mktemp)"
awk -v ns="$NAMESPACE" -v cr="$CR_NAME" -v infra="$INFRA_NAMESPACE" \
    -v domain="$DOMAIN" -v bootstrap="$BOOTSTRAP" \
    -v dbsec="${CR_NAME}-db-credentials" \
    -v cachesec="${CR_NAME}-cache-credentials" \
    -v s3sec="${CR_NAME}-storage-credentials" '
  BEGIN { in_meta=0 }
  /^metadata:/ { in_meta=1 }
  /^spec:/ { in_meta=0 }
  {
    gsub(/cost-byoi-infra/, infra)
    if (domain != "") gsub(/apps\.cluster\.example\.com/, domain)
    gsub(/cost-onprem-kafka-kafka-bootstrap\.kafka\.svc\.cluster\.local:9092/, bootstrap)
    gsub(/secretName: "byoi-db-credentials"/, "secretName: \"" dbsec "\"")
    gsub(/secretName: "byoi-cache-credentials"/, "secretName: \"" cachesec "\"")
    gsub(/secretName: "byoi-s3-credentials"/, "secretName: \"" s3sec "\"")
  }
  in_meta && /^  namespace: cost-byoi$/ { print "  namespace: " ns; next }
  in_meta && /^  name: cost-management$/ { print "  name: " cr; next }
  { print }
' "$SAMPLE_CR" >"$TMP_CR"
"$KUBECTL" apply -f "$TMP_CR"
rm -f "$TMP_CR"

if [[ -z "$DOMAIN" ]]; then
  echo "warning: cluster ingress domain unset; Routes may wait on discovery" >&2
fi

echo "Waiting for CMSC ${NAMESPACE}/${CR_NAME} status.phase=Ready (${CMSC_READY_TIMEOUT})..."
if ! "$KUBECTL" wait "cmsc/${CR_NAME}" -n "${NAMESPACE}" \
  --for=jsonpath='{.status.phase}'=Ready \
  --timeout="${CMSC_READY_TIMEOUT}"; then
  echo "error: CMSC did not reach Ready" >&2
  "$KUBECTL" -n "${NAMESPACE}" get cmsc "${CR_NAME}" \
    -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}' \
    2>/dev/null | redact_filter || true
  exit 1
fi

if [[ "$SKIP_PYTEST" == "1" ]]; then
  echo "[4/4] Skipping pytest (SKIP_PYTEST=1)"
  exit 0
fi

echo "[4/4] pytest --no-venv --no-ui..."
export NAMESPACE HELM_RELEASE_NAME="${CR_NAME}" KEYCLOAK_NAMESPACE
set +e
./scripts/run-pytest.sh --no-venv --no-ui
pytest_rc=$?
set -e
copy_junit
exit "$pytest_rc"
