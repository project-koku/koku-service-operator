#!/usr/bin/env bash
#
# Remove data retained by scripts/seed-test-data.sh or the operator E2E flow.
# The source is deleted through Koku's API; uploaded objects and report records
# are then removed for the source's cluster_id. Provider lifecycle remains under
# Koku's API and is not modified with raw database deletes.
#
# Usage:
#   ORG_ID=1234567 ./scripts/cleanup-test-data.sh \
#     --source-id 123 --cluster-id <cluster-uuid>
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TESTS_DIR="${PROJECT_ROOT}/test/pytest"
VENV_DIR="${TESTS_DIR}/.venv"
PYTHON="${PYTHON:-python3}"

NAMESPACE="${NAMESPACE:-cost-onprem}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
ORG_ID="${ORG_ID:-}"
ACCOUNT_NUMBER="${ACCOUNT_NUMBER:-7890123}"
SOURCE_ID=""
CLUSTER_ID=""
USE_VENV=true

RED=$'\033[0;31m'; BLUE=$'\033[0;34m'; NC=$'\033[0m'
log() { echo "${BLUE}[cleanup-test-data]${NC} $*"; }
err() { echo "${RED}[cleanup-test-data]${NC} $*" >&2; }

usage() {
  sed -n '2,10p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

require_value() {
  local option="$1"
  local value="${2:-}"
  if [[ -z "$value" ]]; then
    err "$option requires a value"
    usage >&2
    exit 2
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-id)
      SOURCE_ID="${2:-}"
      require_value "$1" "$SOURCE_ID"
      shift 2
      ;;
    --cluster-id)
      CLUSTER_ID="${2:-}"
      require_value "$1" "$CLUSTER_ID"
      shift 2
      ;;
    --no-venv)
      USE_VENV=false
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      err "unknown flag: $1"
      usage >&2
      exit 2
      ;;
  esac
done

require_value "ORG_ID environment variable" "$ORG_ID"
require_value "--source-id" "$SOURCE_ID"

command -v oc >/dev/null 2>&1 || { err "oc CLI not found"; exit 1; }
oc whoami >/dev/null 2>&1 || { err "not logged into a cluster (run 'oc login')"; exit 1; }

if [[ "$USE_VENV" == "true" ]]; then
  [[ -d "$VENV_DIR" ]] || { log "creating venv at ${VENV_DIR}"; "$PYTHON" -m venv "$VENV_DIR"; }
  # shellcheck source=/dev/null
  source "${VENV_DIR}/bin/activate"
  pip install --quiet --upgrade pip
  pip install --quiet -r "${TESTS_DIR}/requirements.txt"
  PYTHON=python3
fi

PF_LOG="$(mktemp)"
oc -n "$NAMESPACE" port-forward "svc/${HELM_RELEASE_NAME}-koku-api" :8000 >"$PF_LOG" 2>&1 &
PF_PID=$!
cleanup() { kill "$PF_PID" >/dev/null 2>&1 || true; rm -f "$PF_LOG"; }
trap cleanup EXIT

for _ in $(seq 1 30); do
  grep -q 'Forwarding from' "$PF_LOG" && break
  kill -0 "$PF_PID" 2>/dev/null || { err "port-forward died:"; cat "$PF_LOG" >&2; exit 1; }
  sleep 1
done
PF_PORT="$(sed -n 's/.*Forwarding from 127\.0\.0\.1:\([0-9]*\).*/\1/p' "$PF_LOG" | head -1)"
[[ -n "$PF_PORT" ]] || { err "could not determine port-forward port:"; cat "$PF_LOG" >&2; exit 1; }

TESTS_DIR="$TESTS_DIR" NAMESPACE="$NAMESPACE" HELM_RELEASE_NAME="$HELM_RELEASE_NAME" \
ORG_ID="$ORG_ID" ACCOUNT_NUMBER="$ACCOUNT_NUMBER" SOURCE_ID="$SOURCE_ID" \
CLUSTER_ID="$CLUSTER_ID" KOKU_API_URL="http://localhost:${PF_PORT}/api/cost-management/v1" \
  "$PYTHON" - <<'PY'
import os
import sys

import requests

sys.path.insert(0, os.environ["TESTS_DIR"])

from cleanup import cleanup_database_records, cleanup_s3_data
from conftest import ClusterConfig, s3_config
from utils import create_rh_identity_header, get_pod_by_label

namespace = os.environ["NAMESPACE"]
release = os.environ["HELM_RELEASE_NAME"]
org_id = os.environ["ORG_ID"]
account_number = os.environ["ACCOUNT_NUMBER"]
source_id = os.environ["SOURCE_ID"]
cluster_id_arg = os.environ.get("CLUSTER_ID") or ""
koku_api = os.environ["KOKU_API_URL"].rstrip("/")

session = requests.Session()
session.headers.update({
    "X-Rh-Identity": create_rh_identity_header(org_id, account_number=account_number),
    "Content-Type": "application/json",
})

source_response = session.get(f"{koku_api}/sources/{source_id}", timeout=60)
source_exists = source_response.status_code != 404
if source_exists:
    source_response.raise_for_status()
    source = source_response.json()
else:
    source = {}
cluster_id = cluster_id_arg or source.get("source_ref") or ""
if not cluster_id:
    sys.exit("error: --cluster-id is required when the source has no source_ref")
if source_exists:
    print(f"source={source.get('name', '<unnamed>')} source_id={source_id} cluster_id={cluster_id}")
else:
    print(f"source={source_id} is already absent; cleaning cluster_id={cluster_id}")

cluster_config = ClusterConfig(
    namespace=namespace,
    helm_release_name=release,
    keycloak_namespace=os.environ.get("KEYCLOAK_NAMESPACE", "keycloak"),
)
incomplete = False
s3_factory = getattr(s3_config, "__wrapped__", s3_config)
s3 = s3_factory(cluster_config)
if s3 is None:
    print("warning: S3 configuration was not found; uploaded objects were not removed")
    incomplete = True
else:
    s3_result = cleanup_s3_data(
        endpoint=s3.endpoint,
        access_key=s3.access_key,
        secret_key=s3.secret_key,
        bucket=s3.bucket,
        org_id=org_id,
        cluster_id=cluster_id,
        verify_ssl=s3.verify_ssl,
        addressing_style=s3.addressing_style,
    )
    print(f"S3 cleanup: {s3_result}")
    incomplete = incomplete or bool(s3_result.get("error") or s3_result.get("errors"))

db_pod = get_pod_by_label(namespace, "app.kubernetes.io/component=database")
if db_pod:
    db_result = cleanup_database_records(
        namespace=namespace,
        db_pod=db_pod,
        org_id=org_id,
        cluster_id=cluster_id,
    )
    print(f"database cleanup: {db_result}")
    incomplete = incomplete or bool(db_result.get("errors"))
else:
    print("warning: database pod was not found; report records were not removed")
    incomplete = True

if incomplete:
    sys.exit("error: data cleanup was incomplete; the source was left in place for retry")

if source_exists:
    delete_response = session.delete(f"{koku_api}/sources/{source_id}", timeout=60)
    if delete_response.status_code not in (204, 404):
        sys.exit(
            f"error: source deletion failed: HTTP {delete_response.status_code} "
            f"{delete_response.text[:300]}"
        )
    print("source deleted through the Koku API")
else:
    print("source was already absent")

print("provider lifecycle was left to Koku; no raw provider rows were deleted")
PY

log "done"
