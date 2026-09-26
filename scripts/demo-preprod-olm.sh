#!/usr/bin/env bash
# Pre-prod OLM demo: BYOI dependencies -> Console catalog + CMSC -> tmux + UI.
#
# This deliberately does not build or deploy the operator. It prepares the
# dependencies, renders a CMSC, and prints the console steps a user uses to
# install the published catalog and create the CR. Then run --watch to observe
# reconciliation and open the UI once it is available.
#
# Usage (from repo root):
#   ./scripts/demo-preprod-olm.sh --prepare
#   ./scripts/demo-preprod-olm.sh --console
#   ./scripts/demo-preprod-olm.sh --watch
#   ./scripts/demo-preprod-olm.sh --dry-run
#
# The default catalog is the published file-based catalog image. The console
# creates the Subscription; NAMESPACE is both its install namespace and the
# CMSC namespace.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$ROOT"
# shellcheck disable=SC1091
source "${ROOT}/scripts/lib/demo-preprod.bash"

MODE="prepare"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
NAMESPACE="${NAMESPACE:-}"
CR_NAME="${CR_NAME:-}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-}"
CATALOG_IMG="${CATALOG_IMG:-}"
CATALOG_NAME="${CATALOG_NAME:-koku-service-operator-catalog}"
OPERATOR_VERSION="${OPERATOR_VERSION:-v0.0.1}"
CONSOLE_USERNAME="${CONSOLE_USERNAME:-}"
CONSOLE_PASSWORD="${CONSOLE_PASSWORD:-}"
CR_FILE="${CR_FILE:-}"
TMUX_SESSION="${TMUX_SESSION:-koku-demo-preprod-olm}"
OPEN_BROWSER="${OPEN_BROWSER:-1}"
NO_TMUX="${NO_TMUX:-0}"

usage() {
  sed -n '2,16p' "${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --prepare) MODE="prepare" ;;
    --console) MODE="console" ;;
    --watch) MODE="watch" ;;
    --dry-run) MODE="dry-run" ;;
    --no-tmux) NO_TMUX=1 ;;
    --no-open) OPEN_BROWSER=0 ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "error: unknown flag $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

load_env_file() {
  local file="$1" line key value
  [[ -f "$file" ]] || return 0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ "$line" =~ ^[[:space:]]*# ]] && continue
    [[ -z "${line//[[:space:]]/}" ]] && continue
    line="${line#export }"
    key="${line%%=*}"
    key="${key//[[:space:]]/}"
    value="${line#*=}"
    value="${value#\"}"
    value="${value%\"}"
    value="${value#\'}"
    value="${value%\'}"
    if [[ -z "${!key:-}" ]]; then
      export "${key}=${value}"
    fi
  done <"$file"
}

# The local file is intentionally shared so the two demos target the same
# Cluster Bot without duplicate configuration. Caller-exported values win.
load_env_file "${ROOT}/scripts/demo-preprod.env.example"
load_env_file "${ROOT}/scripts/demo-preprod.local.env"
KUBE_CONTEXT="${KUBE_CONTEXT:-clusterbot}"
NAMESPACE="${NAMESPACE:-cost-byoi}"
CR_NAME="${CR_NAME:-cost-management}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-cost-byoi-infra}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
CATALOG_IMG="${CATALOG_IMG:-quay.io/project-koku/koku-service-operator-catalog:latest}"
CONSOLE_USERNAME="${CONSOLE_USERNAME:-kubeadmin}"
CR_FILE="${CR_FILE:-/tmp/koku-demo-preprod-olm-${NAMESPACE}.yaml}"

print_plan() {
  cat <<EOF
Pre-prod OLM demo
  context:     ${KUBE_CONTEXT}
  app / OLM NS:${NAMESPACE}  (CMSC ${CR_NAME})
  infra NS:   ${INFRA_NAMESPACE}
  kafka NS:   ${KAFKA_NAMESPACE}
  keycloak NS:${KEYCLOAK_NAMESPACE}
  catalog:    ${CATALOG_IMG}
  operator:   ${OPERATOR_VERSION}
  rendered CR:${CR_FILE}

  1. --prepare: deploy Kafka, Postgres, Valkey, MinIO, Keycloak, OAuth and app Secrets
  2. Add the catalog and install the operator in the OpenShift console, then apply the CMSC
  3. --watch: watch CMSC/pods in tmux and open the UI after its rollout
EOF
}

if [[ "$MODE" == "dry-run" ]]; then
  print_plan
  exit 0
fi

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 1
  }
}

need_cmd oc
if ! oc config get-contexts "$KUBE_CONTEXT" >/dev/null 2>&1; then
  echo "error: kube context '${KUBE_CONTEXT}' not found" >&2
  exit 1
fi
oc config use-context "$KUBE_CONTEXT" >/dev/null
require_reachable_cluster oc

DOMAIN="$(read_apps_domain oc)"
KEYCLOAK_ISSUER="https://keycloak-${KEYCLOAK_NAMESPACE}.${DOMAIN}"
UI_URL="https://${CR_NAME}-ui-${NAMESPACE}.${DOMAIN}"
CONSOLE_URL="https://console-openshift-console.${DOMAIN}"

print_console_access() {
  echo "OpenShift Console: ${CONSOLE_URL}"
  echo "Console username: ${CONSOLE_USERNAME}"
  if [[ -n "$CONSOLE_PASSWORD" ]]; then
    echo "Console password: ${CONSOLE_PASSWORD}"
  else
    echo "Console password: retrieve it from the ClusterBot provisioning output"
    echo "  (kube-system/kubeadmin stores only a password hash, not the login password)."
  fi
}

if [[ "$MODE" == "console" ]]; then
  print_console_access
  exit 0
fi

if [[ "$MODE" == "prepare" ]]; then
  if [[ ! -x "${ROOT}/scripts/deploy-rhbk.sh" ]]; then
    echo "error: expected sibling-compatible RHBK script is missing: ${ROOT}/scripts/deploy-rhbk.sh" >&2
    echo "  Set RHBK_SCRIPT to the customer/demo Keycloak provisioner before running this demo." >&2
    exit 1
  fi

  print_console_access
  echo "=== Preparing BYOI dependencies on ${KUBE_CONTEXT} ==="
  KUBE_CONTEXT="$KUBE_CONTEXT" \
    NAMESPACE="$NAMESPACE" CR_NAME="$CR_NAME" \
    INFRA_NAMESPACE="$INFRA_NAMESPACE" KAFKA_NAMESPACE="$KAFKA_NAMESPACE" \
    KEYCLOAK_NAMESPACE="$KEYCLOAK_NAMESPACE" \
    ./scripts/deploy-byoi.sh

  render_preprod_cr \
    "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" \
    "$CR_FILE" "$DOMAIN" "$KEYCLOAK_ISSUER" "$NAMESPACE" "$CR_NAME"

  cat <<EOF

BYOI is ready and the CMSC was rendered to ${CR_FILE}.

Install the operator through the OpenShift console:

  Console URL: ${CONSOLE_URL}

  1. Administrator -> Cluster Settings -> Configuration -> OperatorHub -> Sources.
     Create a CatalogSource named ${CATALOG_NAME} in openshift-marketplace,
     with image ${CATALOG_IMG} and source type grpc.
  2. Wait for the catalog source to be Ready, then open Ecosystem -> Software Catalog.
     Search for Cost Management Service Operator and click Install.
  3. Verify that the catalog offers ${OPERATOR_VERSION}, then choose channel beta,
     installation mode All namespaces on the cluster, installed namespace
     ${NAMESPACE}, and Automatic approval. Click Install.
  4. Wait until Installed Operators shows the CSV as Succeeded, then run:

       oc apply -f ${CR_FILE}

Then start the demo observers:

  ./scripts/demo-preprod-olm.sh --watch
EOF
  exit 0
fi

if ! oc -n "$NAMESPACE" get cmsc "$CR_NAME" >/dev/null 2>&1; then
  echo "error: CMSC ${NAMESPACE}/${CR_NAME} is not present." >&2
  echo "  Run --prepare, install through the Console catalog, then apply ${CR_FILE}." >&2
  exit 1
fi

wait_for_ui() {
  echo "Waiting for deployment/${CR_NAME}-ui in ${NAMESPACE}..."
  until oc -n "$NAMESPACE" rollout status "deployment/${CR_NAME}-ui" --timeout=30s; do
    sleep 5
  done
  echo "UI ready: ${UI_URL}"
  echo "UI login: admin / admin"
  if [[ "$OPEN_BROWSER" == "1" ]] && command -v open >/dev/null 2>&1; then
    open "$UI_URL"
  fi
}

if [[ "$NO_TMUX" == "1" ]]; then
  wait_for_ui
  exit 0
fi

need_cmd tmux
tmux has-session -t "$TMUX_SESSION" 2>/dev/null && tmux kill-session -t "$TMUX_SESSION"
tmux new-session -d -s "$TMUX_SESSION" -n demo
tmux send-keys -t "${TMUX_SESSION}:demo.0" \
  "oc -n $(printf %q "$NAMESPACE") get cmsc $(printf %q "$CR_NAME") -w" C-m
tmux split-window -h -t "${TMUX_SESSION}:demo" \
  "oc -n $(printf %q "$NAMESPACE") get pods -w"
tmux split-window -v -t "${TMUX_SESSION}:demo.1" \
  "cd $(printf %q "$ROOT") && NO_TMUX=1 OPEN_BROWSER=$(printf %q "$OPEN_BROWSER") $(printf %q "$SCRIPT_DIR/$(basename "$0")") --watch"
tmux select-pane -t "${TMUX_SESSION}:demo.0"
tmux attach-session -t "$TMUX_SESSION"
