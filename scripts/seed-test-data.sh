#!/usr/bin/env bash
#
# Seed cost data into a running Cost On-Prem deployment WITHOUT running the
# pytest suite. Registers an OpenShift source (via a port-forward to the
# internal Koku API), generates NISE OCP data, and uploads it through the
# gateway/ingress. Reuses the helpers in test/pytest (e2e_helpers.py,
# conftest.py, utils.py) so it stays in sync with the tests.
#
# Usage (from repo root):
#   ./scripts/seed-test-data.sh [--days N] [--clusters N] [--source-name NAME]
#                               [--org-id ID] [--no-venv]
#
# Environment:
#   NAMESPACE           CR / app namespace          (default: cost-onprem)
#   HELM_RELEASE_NAME   CR name / resource prefix   (default: cost-onprem)
#   KEYCLOAK_NAMESPACE  Keycloak namespace          (default: keycloak)
#   PYTHON             Python interpreter           (default: python3)
#
# `oc` must be logged in to the target cluster. masu processes the upload
# asynchronously off the Kafka topic; data appears in the UI a few minutes later.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TESTS_DIR="${PROJECT_ROOT}/test/pytest"
VENV_DIR="${TESTS_DIR}/.venv"
PYTHON="${PYTHON:-python3}"

NAMESPACE="${NAMESPACE:-cost-onprem}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"

DAYS=3
CLUSTERS=1
SOURCE_NAME=""
ORG_ID_ARG="${ORG_ID:-}"   # empty => derive from the JWT org_id claim
USE_VENV=true

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; BLUE=$'\033[0;34m'; NC=$'\033[0m'
log() { echo "${BLUE}[seed-test-data]${NC} $*"; }
ok()  { echo "${GREEN}[seed-test-data]${NC} $*"; }
err() { echo "${RED}[seed-test-data]${NC} $*" >&2; }

usage() { sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --days) DAYS="$2"; shift 2 ;;
    --clusters) CLUSTERS="$2"; shift 2 ;;
    --source-name) SOURCE_NAME="$2"; shift 2 ;;
    --org-id) ORG_ID_ARG="$2"; shift 2 ;;
    --no-venv) USE_VENV=false; shift ;;
    -h|--help) usage; exit 0 ;;
    *) err "unknown flag: $1"; usage >&2; exit 2 ;;
  esac
done

command -v oc >/dev/null 2>&1 || { err "oc CLI not found"; exit 1; }
oc whoami >/dev/null 2>&1 || { err "not logged into a cluster (run 'oc login')"; exit 1; }
oc get deployment -n "$NAMESPACE" "${HELM_RELEASE_NAME}-koku-api" >/dev/null 2>&1 || {
  err "deployment ${HELM_RELEASE_NAME}-koku-api not found in namespace ${NAMESPACE}"
  err "set NAMESPACE / HELM_RELEASE_NAME to match the CostManagementServiceConfig"
  exit 1
}

if [[ "$USE_VENV" == "true" ]]; then
  [[ -d "$VENV_DIR" ]] || { log "creating venv at ${VENV_DIR}"; "$PYTHON" -m venv "$VENV_DIR"; }
  # shellcheck source=/dev/null
  source "${VENV_DIR}/bin/activate"
  pip install --quiet --upgrade pip
  pip install --quiet -r "${TESTS_DIR}/requirements.txt"
  PYTHON=python3
fi

# --- port-forward the internal Koku API (source registration path) -----------
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
KOKU_API_URL="http://localhost:${PF_PORT}/api/cost-management/v1"

log "namespace=${NAMESPACE} release=${HELM_RELEASE_NAME} keycloak=${KEYCLOAK_NAMESPACE}"
log "days=${DAYS} clusters=${CLUSTERS} org_id=${ORG_ID_ARG:-<from JWT>}  koku-api via localhost:${PF_PORT}"

TESTS_DIR="$TESTS_DIR" NAMESPACE="$NAMESPACE" HELM_RELEASE_NAME="$HELM_RELEASE_NAME" \
KEYCLOAK_NAMESPACE="$KEYCLOAK_NAMESPACE" DAYS="$DAYS" CLUSTERS="$CLUSTERS" \
SOURCE_NAME="$SOURCE_NAME" ORG_ID_ARG="$ORG_ID_ARG" KOKU_API_URL="$KOKU_API_URL" "$PYTHON" - <<'PY'
import os
import sys
import time
import uuid
import tempfile
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.environ["TESTS_DIR"])

import requests
import urllib3
urllib3.disable_warnings()

from conftest import (
    ClusterConfig,
    KeycloakConfig,
    obtain_jwt_token,
    decode_jwt_payload,
)
from utils import (
    get_route_url,
    get_secret_value,
    run_oc_command,
    get_pod_by_label,
    exec_in_pod_raw,
    create_rh_identity_header,
    create_upload_package_from_files,
)
from e2e_helpers import (
    DEFAULT_S3_BUCKET,
    ensure_nise_available,
    generate_nise_data,
    upload_with_retry,
)
from rbac_bootstrap_scripts import render_bootstrap_script

ns = os.environ["NAMESPACE"]
release = os.environ["HELM_RELEASE_NAME"]
kc_ns = os.environ["KEYCLOAK_NAMESPACE"]
days = int(os.environ["DAYS"])
clusters = int(os.environ["CLUSTERS"])
org_id_arg = os.environ.get("ORG_ID_ARG") or ""  # empty unless --org-id given
source_name_arg = os.environ.get("SOURCE_NAME") or ""
koku_api = os.environ["KOKU_API_URL"].rstrip("/")

ClusterConfig(namespace=ns, helm_release_name=release, keycloak_namespace=kc_ns)

# --- Keycloak client-credentials JWT (mirrors conftest.keycloak_config) ------
kc_url = get_route_url(kc_ns, "keycloak")
if not kc_url:
    sys.exit(f"error: Keycloak route not found in namespace {kc_ns}")
client_id = "cost-management-operator"
client_secret = None
for name in (
    f"keycloak-client-secret-{client_id}",
    "keycloak-client-secret-cost-management-service-account",
    f"credential-{client_id}",
    f"keycloak-client-{client_id}",
    f"{client_id}-secret",
):
    client_secret = get_secret_value(kc_ns, name, "CLIENT_SECRET")
    if client_secret:
        break
if not client_secret:
    sys.exit(f"error: Keycloak client secret for {client_id} not found in {kc_ns}")
jwt = obtain_jwt_token(KeycloakConfig(url=kc_url, client_id=client_id, client_secret=client_secret))
print(f"  JWT obtained (client_id={client_id})")

# --- gateway / ingress upload URL (mirrors conftest.gateway_url / ingress_url) -
gw = get_route_url(ns, f"{release}-api")
if not gw:
    sys.exit(f"error: gateway route '{release}-api' not found in namespace {ns}")
path = run_oc_command(
    ["get", "route", f"{release}-api", "-n", ns, "-o", "jsonpath={.spec.path}"],
    check=False,
).stdout.strip().rstrip("/")
gateway_url = (f"{gw}{path}" if path else gw).rstrip("/")
ingress_url = f"{gateway_url}/ingress" if gateway_url.endswith("/api") else f"{gateway_url}/api/ingress"
upload_url = f"{ingress_url}/v1/upload"
print(f"  gateway={gateway_url}")

# --- RBAC bootstrap: provision the org tenant + a Cost Administrator ---------
# koku's Sources API calls RBAC on writes; on a fresh deployment RBAC has no
# tenant/principal for this org and returns 424. This mirrors the e2e conftest
# bootstrap and is a no-op (get_or_create) where RBAC is already seeded.
try:
    claims = decode_jwt_payload(jwt.access_token)
except Exception:
    claims = {}
# Use the org_id the ingress/JWT path stamps onto uploaded payloads, else Koku
# creates the provider under one tenant and processes the report under another
# ("Received unexpected OCP report").
org_id = org_id_arg or claims.get("org_id") or "org1234567"
acct_number = claims.get("account_number") or "7890123"
print(f"  org_id={org_id} account_number={acct_number}")
sa_default = f"service-account-{client_id}"
sa_usernames = list(dict.fromkeys([
    claims.get("preferred_username") or claims.get("sub") or sa_default,
    "cost-mgmt-operator",
    sa_default,
]))
try:
    requests.get(f"{gateway_url}/cost-management/v1/status/",
                 headers={"Authorization": f"Bearer {jwt.access_token}"},
                 verify=False, timeout=30)
except Exception as e:
    print(f"  (tenant trigger request failed: {e})")
time.sleep(3)
rbac_pod = get_pod_by_label(ns, "app.kubernetes.io/component=rbac-api")
if not rbac_pod:
    print("  warning: rbac-api pod not found; skipping RBAC bootstrap")
else:
    r1 = exec_in_pod_raw(
        ns, rbac_pod,
        ["python", "/opt/rbac/rbac/manage.py", "shell", "-c",
         render_bootstrap_script(sa_usernames, org_id, acct_number)],
        timeout=120,
    )
    print(f"  RBAC bootstrap: {'ok' if r1.returncode == 0 else 'FAILED rc=' + str(r1.returncode)}")
    if r1.returncode != 0:
        print((r1.stderr or "").strip()[:400])
    r2 = exec_in_pod_raw(
        ns, rbac_pod,
        ["python", "/opt/rbac/rbac/manage.py", "bootstrap_tenants", "--org-id", org_id, "--force"],
        timeout=120,
    )
    print(f"  RBAC bootstrap_tenants: {'ok' if r2.returncode == 0 else 'FAILED rc=' + str(r2.returncode)}")
    time.sleep(3)

# --- internal Koku API session (X-Rh-Identity admin, via port-forward) -------
identity = create_rh_identity_header(org_id, account_number=acct_number)  # is_org_admin=True
koku = requests.Session()
koku.headers.update({"X-Rh-Identity": identity, "Content-Type": "application/json"})

st = koku.get(f"{koku_api}/source_types", timeout=30)
st.raise_for_status()
ocp_type_id = next((s["id"] for s in st.json().get("data", []) if s.get("name") == "openshift"), None)
if not ocp_type_id:
    sys.exit("error: 'openshift' source type not found in Koku")
at = koku.get(f"{koku_api}/application_types", timeout=30)
app_id = next(
    (a["id"] for a in (at.json().get("data", []) if at.ok else [])
     if a.get("name") == "/insights/platform/cost-management"),
    None,
)

def register(source_name, cluster_id):
    payload = {"name": source_name, "source_type_id": ocp_type_id, "source_ref": cluster_id}
    last = None
    delay = 5
    for attempt in range(5):
        if attempt:
            time.sleep(delay)
            delay = min(delay * 2, 30)
        r = koku.post(f"{koku_api}/sources", json=payload, timeout=60)
        if r.status_code in (200, 201) and r.json().get("id"):
            sid = r.json()["id"]
            if app_id is not None:
                # extra.cluster_id is what wires the Koku provider to the cluster
                ar = koku.post(
                    f"{koku_api}/applications",
                    json={
                        "source_id": sid,
                        "application_type_id": app_id,
                        "extra": {"bucket": DEFAULT_S3_BUCKET, "cluster_id": cluster_id},
                    },
                    timeout=60,
                )
                if not ar.ok:
                    print(f"  warning: application link failed: HTTP {ar.status_code} {ar.text[:200]}")
            return sid
        last = f"HTTP {r.status_code}: {r.text[:200]}"
        if 400 <= r.status_code < 500 and r.status_code != 409:
            break
    sys.exit(f"error: source creation failed: {last}")

ensure_nise_available()
up = requests.Session()
up.verify = False

end = datetime.now(timezone.utc)
start = end - timedelta(days=days)
seeded = []

for i in range(clusters):
    cluster_id = str(uuid.uuid4())
    name = source_name_arg or f"seed-{cluster_id[:8]}"
    if clusters > 1 and source_name_arg:
        name = f"{source_name_arg}-{i + 1:02d}"
    print(f"\ncluster {i + 1}/{clusters}: {name} ({cluster_id})")

    sid = register(name, cluster_id)
    print(f"  source + provider registered (source_id={sid})")
    time.sleep(10)  # let Koku finish wiring the provider before the upload

    tmp = tempfile.mkdtemp(prefix="seed-nise-")
    files = generate_nise_data(cluster_id, start, end, tmp)
    pkg = create_upload_package_from_files(
        files.get("pod_usage_files", []),
        files.get("ros_usage_files", []),
        cluster_id,
        start_date=start,
        end_date=end,
        node_label_files=files.get("node_label_files") or None,
        namespace_label_files=files.get("namespace_label_files") or None,
    )
    resp = upload_with_retry(up, upload_url, pkg, jwt.authorization_header)
    print(f"  upload -> HTTP {resp.status_code}")
    seeded.append((name, cluster_id, sid))

print("\nseeded:")
for name, cid, sid in seeded:
    print(f"  {name}  source_id={sid}  cluster_id={cid}")
print(f"\n{days} day(s) of data for {clusters} cluster(s). masu processes the "
      "upload asynchronously; data appears in the UI in a few minutes.")
PY

ok "done"
