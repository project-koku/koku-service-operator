#!/usr/bin/env bash
# Full pre-prod demo: BYOI (AMQ Streams + Keycloak) → in-cluster operator → UI.
#
# Usage (from repo root or hack/):
#   ./hack/demo-preprod.sh
#   ./hack/demo-preprod.sh --dry-run
#   ./hack/demo-preprod.sh --reset          # delete app/infra/kafka/keycloak NS first
#   ./hack/demo-preprod.sh --no-tmux
#   ./hack/demo-preprod.sh --rebuild        # force operator image rebuild
#   ./hack/demo-preprod.sh --crc            # target local CRC (arm64, single node)
#
# Settings: env vars, then hack/demo-preprod.local.env, then
# hack/demo-preprod.env.example (and hack/demo-preprod.crc.env with --crc).
#
# tmux (default): left pane = numbered steps; top-right = kubectl klock pods
# in NAMESPACE; bottom-right = kubectl klock pods in KAFKA_NAMESPACE.
# When the UI Deployment is Ready, `open` the UI Route (macOS).
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SCRIPT="${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")"
cd "$ROOT"
# shellcheck disable=SC1091
source "${ROOT}/hack/lib/demo-preprod.bash"

DEMO_RESET="${DEMO_RESET:-0}"
DEMO_DRY_RUN="${DEMO_DRY_RUN:-0}"
DEMO_NO_TMUX="${DEMO_NO_TMUX:-0}"
DEMO_REBUILD="${DEMO_REBUILD:-0}"
DEMO_NO_OPEN="${DEMO_NO_OPEN:-0}"
DEMO_CRC="${DEMO_CRC:-0}"

usage() {
  sed -n '2,18p' "$SCRIPT" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --reset) DEMO_RESET=1 ;;
    --dry-run) DEMO_DRY_RUN=1 ;;
    --no-tmux) DEMO_NO_TMUX=1 ;;
    --rebuild) DEMO_REBUILD=1 ;;
    --no-open) DEMO_NO_OPEN=1 ;;
    --crc) DEMO_CRC=1 ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "error: unknown flag $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

# Load KEY=VALUE from a file without clobbering variables already in the environment.
load_env_file() {
  local file="$1" line key val
  [[ -f "$file" ]] || return 0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ "$line" =~ ^[[:space:]]*# ]] && continue
    [[ -z "${line//[[:space:]]/}" ]] && continue
    line="${line#export }"
    key="${line%%=*}"
    key="${key//[[:space:]]/}"
    val="${line#*=}"
    val="${val#\"}"
    val="${val%\"}"
    val="${val#\'}"
    val="${val%\'}"
    if [[ -z "${!key:-}" ]]; then
      export "${key}=${val}"
    fi
  done <"$file"
}

# --crc: layer the CRC/arm64 profile under the normal files. load_env_file only
# sets vars that are still unset, so real env > .local.env > .env.example >
# .crc.env, and the amd64 path (no --crc) never reads this file.
if [[ "$DEMO_CRC" == "1" ]]; then
  load_env_file "${ROOT}/hack/demo-preprod.crc.env"
fi
load_env_file "${ROOT}/hack/demo-preprod.env.example"
load_env_file "${ROOT}/hack/demo-preprod.local.env"

if [[ "$DEMO_CRC" == "1" ]]; then
  KUBE_CONTEXT="${KUBE_CONTEXT:-crc}"
else
  KUBE_CONTEXT="${KUBE_CONTEXT:-clusterbot}"
fi
NAMESPACE="${NAMESPACE:-cost-byoi}"
CR_NAME="${CR_NAME:-cost-management}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-cost-byoi-infra}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
KAFKA_CLUSTER_NAME="${KAFKA_CLUSTER_NAME:-cost-onprem-kafka}"
OPEN_BROWSER="${OPEN_BROWSER:-1}"
BUILD_MODE="${BUILD_MODE:-auto}"
IMAGE_TAG="${IMAGE_TAG:-preprod}"
TMUX_SESSION="${TMUX_SESSION:-koku-demo-preprod}"
WATCHER="${WATCHER:-klock}"
LOG_LEVEL="${LOG_LEVEL:-INFO}"
KUBECTL="${KUBECTL:-oc}"

GIT_COMMON=""
if git -C "$ROOT" rev-parse --git-common-dir >/dev/null 2>&1; then
  GIT_COMMON="$(git -C "$ROOT" rev-parse --git-common-dir)"
  if [[ "$GIT_COMMON" != /* ]]; then
    GIT_COMMON="$(cd "$ROOT/${GIT_COMMON}" && pwd)"
  fi
fi
if [[ -z "${CHART_ROOT:-}" ]]; then
  CHART_ROOT="$(default_chart_root "$ROOT" "$GIT_COMMON")"
fi
export CHART_ROOT NAMESPACE CR_NAME INFRA_NAMESPACE KAFKA_NAMESPACE KEYCLOAK_NAMESPACE LOG_LEVEL

IMG="${IMG:-image-registry.openshift-image-registry.svc:5000/${NAMESPACE}/koku-service-operator:${IMAGE_TAG}}"
export IMG

STEP_N=0
STEP_TOTAL=8

if [[ -t 1 ]]; then
  C_BOLD="$(tput bold 2>/dev/null || true)"
  C_CYAN="$(tput setaf 6 2>/dev/null || true)"
  C_GREEN="$(tput setaf 2 2>/dev/null || true)"
  C_YELLOW="$(tput setaf 3 2>/dev/null || true)"
  C_RESET="$(tput sgr0 2>/dev/null || true)"
else
  C_BOLD="" C_CYAN="" C_GREEN="" C_YELLOW="" C_RESET=""
fi

step() {
  STEP_N=$((STEP_N + 1))
  echo ""
  echo "${C_BOLD}${C_CYAN}================================================================${C_RESET}"
  echo "${C_BOLD}${C_CYAN}  [${STEP_N}/${STEP_TOTAL}] $*${C_RESET}"
  echo "${C_BOLD}${C_CYAN}================================================================${C_RESET}"
}

skip() {
  echo "${C_YELLOW}  -> skip: $*${C_RESET}"
}

ok() {
  echo "${C_GREEN}  -> $*${C_RESET}"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: required command not found: $1" >&2
    exit 1
  }
}

klock_cmd() {
  if command -v kubectl-klock >/dev/null 2>&1 || "$KUBECTL" klock -h >/dev/null 2>&1; then
    echo "$KUBECTL klock"
    return 0
  fi
  return 1
}

print_plan() {
  cat <<EOF
Pre-prod demo plan
  context:     ${KUBE_CONTEXT}
  app NS:      ${NAMESPACE}  (CR ${CR_NAME})
  infra NS:    ${INFRA_NAMESPACE}
  kafka NS:    ${KAFKA_NAMESPACE}
  keycloak NS: ${KEYCLOAK_NAMESPACE}
  CHART_ROOT:  ${CHART_ROOT}
  IMG:         ${IMG}
  BUILD_MODE:  ${BUILD_MODE}
  watcher:     ${WATCHER}
  tmux:        ${TMUX_SESSION}  (NO_TMUX=${DEMO_NO_TMUX})
  reset:       ${DEMO_RESET}
  rebuild:     ${DEMO_REBUILD}
  crc mode:    ${DEMO_CRC}$([[ "$DEMO_CRC" == "1" ]] && echo "  (arm64 image overrides + single-node Kafka from hack/demo-preprod.crc.env)")
  open UI:     ${OPEN_BROWSER} (NO_OPEN=${DEMO_NO_OPEN})

  [1/8] Preflight (context, oc, CHART_ROOT, StorageClass)
  [2/8] --reset: delete ${NAMESPACE}, ${INFRA_NAMESPACE}, ${KAFKA_NAMESPACE}, ${KEYCLOAK_NAMESPACE}
  [3/8] BYOI (./hack/deploy-byoi.sh) — skip inner stages that are already Ready
  [4/8] Operator image (${BUILD_MODE}: docker or OpenShift binary build)
  [5/8] In-cluster operator (./hack/deploy-incluster.sh)
  [6/8] Apply CR (clusterDomain + Keycloak issuerURL + insecureSkipVerify, ROS off)
  [7/8] Wait until UI Deployment is Ready
  [8/8] open the UI Route
EOF
}

if [[ "$DEMO_DRY_RUN" == "1" ]]; then
  print_plan
  exit 0
fi

# --- tmux layout (outer process) ---------------------------------------------
start_tmux() {
  need_cmd tmux
  local klock inner
  klock="$(klock_cmd || true)"
  if [[ "$WATCHER" == "klock" && -z "$klock" ]]; then
    echo "warning: kubectl klock not found; watcher panes will use oc get -w" >&2
    klock=""
  fi

  tmux has-session -t "$TMUX_SESSION" 2>/dev/null && tmux kill-session -t "$TMUX_SESSION"
  tmux new-session -d -s "$TMUX_SESSION" -n demo
  tmux set-option -t "$TMUX_SESSION" pane-border-status top
  tmux set-option -t "$TMUX_SESSION" mouse on
  tmux split-window -h -t "${TMUX_SESSION}:demo"
  tmux split-window -v -t "${TMUX_SESSION}:demo.1"

  tmux select-pane -t "${TMUX_SESSION}:demo.0" -T "steps"
  tmux select-pane -t "${TMUX_SESSION}:demo.1" -T "klock pods ${NAMESPACE}"
  tmux select-pane -t "${TMUX_SESSION}:demo.2" -T "klock pods ${KAFKA_NAMESPACE}"

  local watch1 watch2
  klock_watch() {
    local ns="$1"
    if [[ -n "$klock" ]]; then
      echo "${klock} pods -n $(printf %q "$ns") --context $(printf %q "$KUBE_CONTEXT")"
    else
      echo "$KUBECTL get pods -n $(printf %q "$ns") --context $(printf %q "$KUBE_CONTEXT") -w"
    fi
  }
  if [[ "$WATCHER" == "k9s" ]] && command -v k9s >/dev/null 2>&1; then
    watch1="k9s --context $(printf %q "$KUBE_CONTEXT") -n $(printf %q "$NAMESPACE")"
  else
    watch1="$(klock_watch "$NAMESPACE")"
  fi
  watch2="$(klock_watch "$KAFKA_NAMESPACE")"

  tmux send-keys -t "${TMUX_SESSION}:demo.1" "until ${watch1}; do echo 'watcher retry in 3s…'; sleep 3; done" C-m
  tmux send-keys -t "${TMUX_SESSION}:demo.2" "until ${watch2}; do echo 'watcher retry in 3s…'; sleep 3; done" C-m

  inner="cd $(printf %q "$ROOT") && DEMO_INNER=1 DEMO_RESET=$(printf %q "$DEMO_RESET") DEMO_REBUILD=$(printf %q "$DEMO_REBUILD") DEMO_NO_OPEN=$(printf %q "$DEMO_NO_OPEN") DEMO_CRC=$(printf %q "$DEMO_CRC") $(printf %q "$ROOT/hack/demo-preprod.sh")"
  tmux send-keys -t "${TMUX_SESSION}:demo.0" "$inner" C-m
  tmux select-pane -t "${TMUX_SESSION}:demo.0"

  if [[ -n "${TMUX:-}" ]]; then
    tmux switch-client -t "$TMUX_SESSION"
  else
    tmux attach-session -t "$TMUX_SESSION"
  fi
}

if [[ -z "${DEMO_INNER:-}" && "$DEMO_NO_TMUX" != "1" ]] && command -v tmux >/dev/null 2>&1; then
  start_tmux
  exit 0
fi

# --- inner / no-tmux path ----------------------------------------------------
need_cmd "$KUBECTL"
need_cmd python3
need_cmd openssl

step "Preflight"
echo "Cluster context: ${KUBE_CONTEXT}"
if ! "$KUBECTL" config get-contexts "$KUBE_CONTEXT" >/dev/null 2>&1; then
  echo "error: kube context '${KUBE_CONTEXT}' not found" >&2
  "$KUBECTL" config get-contexts >&2 || true
  exit 1
fi
"$KUBECTL" config use-context "$KUBE_CONTEXT" >/dev/null
require_reachable_cluster "$KUBECTL" || exit 1
ok "using $($KUBECTL config current-context) — $($KUBECTL whoami --show-server)"

if [[ "$DEMO_CRC" == "1" ]]; then
  NODE_ARCH="$("$KUBECTL" get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || true)"
  if [[ "$NODE_ARCH" != "arm64" ]]; then
    echo "warning: --crc expects an arm64 node but the cluster reports '${NODE_ARCH:-unknown}'." >&2
    echo "  The arm64 image overrides in hack/demo-preprod.crc.env may not match this node." >&2
  else
    ok "node architecture ${NODE_ARCH} (arm64 image overrides from hack/demo-preprod.crc.env)"
  fi
fi

if [[ ! -x "${CHART_ROOT}/scripts/deploy-rhbk.sh" ]]; then
  echo "error: CHART_ROOT has no scripts/deploy-rhbk.sh: ${CHART_ROOT}" >&2
  echo "  copy hack/demo-preprod.env.example → hack/demo-preprod.local.env and set CHART_ROOT" >&2
  exit 1
fi
ok "CHART_ROOT=${CHART_ROOT}"

DOMAIN="$(read_apps_domain "$KUBECTL")" || exit 1
ok "apps domain=${DOMAIN}"
KEYCLOAK_ISSUER="https://keycloak-${KEYCLOAK_NAMESPACE}.${DOMAIN}"
UI_URL="https://${CR_NAME}-ui-${NAMESPACE}.${DOMAIN}"

# Helpers used by --reset and later skip-if-ready checks. Defined before reset
# because deleting the app namespace kills the operator; we must delete the CR
# first while it can still clear its finalizer.
need_ns() {
  local ns="$1"
  if ! "$KUBECTL" get ns "$ns" >/dev/null 2>&1; then
    "$KUBECTL" create ns "$ns"
  fi
}
operator_ready() {
  [[ "$("$KUBECTL" -n "$NAMESPACE" get deploy koku-service-operator -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo 0)" != "0" ]]
}
clear_finalizers() {
  local ns="$1" kind="$2"
  local obj
  while read -r obj; do
    [[ -z "$obj" ]] && continue
    echo "    strip finalizers: ${ns}/${obj}"
    "$KUBECTL" patch "$obj" -n "$ns" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null || true
  done < <("$KUBECTL" get "$kind" -n "$ns" -o name 2>/dev/null || true)
}
wait_ns_gone() {
  local ns="$1" timeout="${2:-180}"
  local end=$((SECONDS + timeout))
  while (( SECONDS < end )); do
    "$KUBECTL" get ns "$ns" >/dev/null 2>&1 || return 0
    sleep 2
  done
  return 1
}
delete_cr_or_strip() {
  local ns="$1" kind="$2" name="${3:-}"
  local sel
  if [[ -n "$name" ]]; then
    "$KUBECTL" get "$kind" "$name" -n "$ns" >/dev/null 2>&1 || return 0
    sel=("$kind" "$name")
  else
    "$KUBECTL" get "$kind" -n "$ns" -o name 2>/dev/null | grep -q . || return 0
    sel=("$kind" --all)
  fi
  echo "  deleting ${kind}${name:+/}${name} in ${ns}…"
  if ! "$KUBECTL" -n "$ns" delete "${sel[@]}" --timeout=120s >/dev/null 2>&1; then
    echo "  ${kind} delete timed out — stripping finalizers"
    if [[ -n "$name" ]]; then
      "$KUBECTL" patch "$kind" "$name" -n "$ns" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null || true
    else
      clear_finalizers "$ns" "$kind"
    fi
  fi
}
delete_ns() {
  local ns="$1"
  if ! "$KUBECTL" get ns "$ns" >/dev/null 2>&1; then
    skip "namespace ${ns} already gone"
    return 0
  fi
  local phase
  phase="$("$KUBECTL" get ns "$ns" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  if [[ "$phase" != "Terminating" ]]; then
    echo "  deleting namespace ${ns}…"
    "$KUBECTL" delete ns "$ns" --wait=false >/dev/null
  else
    echo "  namespace ${ns} already Terminating"
  fi
  if wait_ns_gone "$ns" 90; then
    ok "namespace ${ns} gone"
    return 0
  fi
  echo "  ${ns} still terminating — stripping leftover finalizers (operator likely already gone)"
  local kind
  for kind in \
    costmanagementserviceconfigs.service.costmanagement.openshift.io \
    kafka kafkanodepool kafkatopic \
    keycloak keycloakrealmimport \
    persistentvolumeclaims; do
    clear_finalizers "$ns" "$kind"
  done
  if wait_ns_gone "$ns" 90; then
    ok "namespace ${ns} gone"
    return 0
  fi
  echo "error: namespace ${ns} still stuck in Terminating" >&2
  "$KUBECTL" get ns "$ns" -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason} {.message}{"\n"}{end}' >&2 || true
  return 1
}

step "Reset namespaces (optional)"
if [[ "$DEMO_RESET" == "1" ]]; then
  # CMSC finalizer requires the operator pod. Deleting the namespace first
  # kills the operator and leaves the CR (and NS) stuck in Terminating.
  if "$KUBECTL" -n "$NAMESPACE" get cmsc "$CR_NAME" >/dev/null 2>&1; then
    if operator_ready; then
      echo "  deleting CMSC ${NAMESPACE}/${CR_NAME} so the operator can clear its finalizer…"
      "$KUBECTL" -n "$NAMESPACE" delete cmsc "$CR_NAME" --timeout=180s || true
    fi
    if "$KUBECTL" -n "$NAMESPACE" get cmsc "$CR_NAME" >/dev/null 2>&1; then
      echo "  operator not running or CMSC still present — stripping finalizer"
      "$KUBECTL" -n "$NAMESPACE" patch cmsc "$CR_NAME" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null || true
    fi
  fi
  "$KUBECTL" delete consolelink "${CR_NAME}-cost-management" --ignore-not-found=true >/dev/null || true
  "$KUBECTL" delete clusterrole,clusterrolebinding -l "app.kubernetes.io/instance=${CR_NAME}" --ignore-not-found=true >/dev/null || true

  delete_cr_or_strip "$KEYCLOAK_NAMESPACE" keycloakrealmimport
  delete_cr_or_strip "$KEYCLOAK_NAMESPACE" keycloak keycloak
  delete_cr_or_strip "$KAFKA_NAMESPACE" kafkatopic
  delete_cr_or_strip "$KAFKA_NAMESPACE" kafka "$KAFKA_CLUSTER_NAME"
  delete_cr_or_strip "$KAFKA_NAMESPACE" kafkanodepool

  delete_ns "$NAMESPACE"
  delete_ns "$INFRA_NAMESPACE"
  delete_ns "$KEYCLOAK_NAMESPACE"
  delete_ns "$KAFKA_NAMESPACE"
  ok "namespaces removed"
else
  skip "pass --reset to delete ${NAMESPACE}, ${INFRA_NAMESPACE}, ${KAFKA_NAMESPACE}, ${KEYCLOAK_NAMESPACE}"
fi

# Pre-create so klock panes have a namespace from the start (empty table, not
# NotFound). deploy-kafka.sh / deploy-rhbk.sh will then warn that the NS
# already exists — that is expected and harmless.
need_ns "$NAMESPACE"
need_ns "$INFRA_NAMESPACE"
need_ns "$KAFKA_NAMESPACE"
need_ns "$KEYCLOAK_NAMESPACE"

kafka_ready() {
  [[ "$("$KUBECTL" -n "$KAFKA_NAMESPACE" get kafka "$KAFKA_CLUSTER_NAME" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)" == "True" ]]
}
infra_ready() {
  "$KUBECTL" -n "$INFRA_NAMESPACE" get deploy postgresql >/dev/null 2>&1 \
    && [[ "$("$KUBECTL" -n "$INFRA_NAMESPACE" get deploy postgresql -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo 0)" != "0" ]]
}
keycloak_ready() {
  [[ "$("$KUBECTL" -n "$KEYCLOAK_NAMESPACE" get keycloak keycloak -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)" == "True" ]]
}
oauth_secret_ready() {
  "$KUBECTL" -n "$NAMESPACE" get secret "${CR_NAME}-ui-oauth-client" >/dev/null 2>&1
}
image_present() {
  "$KUBECTL" -n "$NAMESPACE" get imagestreamtag "koku-service-operator:${IMAGE_TAG}" >/dev/null 2>&1
}
ui_deploy_ready() {
  [[ "$("$KUBECTL" -n "$NAMESPACE" get deploy "${CR_NAME}-ui" -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo 0)" != "0" ]]
}

# RHBK / Keycloak operator: the channel deploy-rhbk.sh pins (stable-v22) ships
# amd64-only operator images. On the arm64 CRC node we pre-create the
# Subscription on an arm64-capable channel (stable-v26); deploy-rhbk.sh then
# finds it and skips its own create. No-op on amd64 / clusterbot.
ensure_rhbk_channel() {
  local ns="$1" channel="$2"
  need_ns "$ns"
  "$KUBECTL" apply -f - >/dev/null <<EOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: rhbk-operator-group
  namespace: ${ns}
spec:
  targetNamespaces:
  - ${ns}
EOF
  local cur
  cur="$("$KUBECTL" -n "$ns" get subscription rhbk-operator -o jsonpath='{.spec.channel}' 2>/dev/null || true)"
  if [[ -z "$cur" ]]; then
    echo "  creating RHBK Subscription on channel ${channel} (arm64)"
    "$KUBECTL" apply -f - >/dev/null <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: rhbk-operator
  namespace: ${ns}
spec:
  channel: ${channel}
  installPlanApproval: Automatic
  name: rhbk-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF
  elif [[ "$cur" != "$channel" ]]; then
    echo "  repointing RHBK Subscription ${cur} -> ${channel} (arm64)"
    "$KUBECTL" -n "$ns" patch subscription rhbk-operator --type=merge \
      -p "{\"spec\":{\"channel\":\"${channel}\"}}" >/dev/null
    "$KUBECTL" -n "$ns" delete csv -l "operators.coreos.com/rhbk-operator.${ns}" --ignore-not-found >/dev/null 2>&1 || true
  else
    echo "  RHBK Subscription already on channel ${channel}"
  fi
  echo "  waiting for deployment/rhbk-operator to be Available…"
  local end=$((SECONDS + 300))
  while (( SECONDS < end )); do
    if [[ "$("$KUBECTL" -n "$ns" get deploy rhbk-operator -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" == "1" ]]; then
      ok "RHBK operator ready (channel ${channel})"
      return 0
    fi
    sleep 5
  done
  echo "error: RHBK operator (channel ${channel}) did not become ready in 5m" >&2
  "$KUBECTL" -n "$ns" get csv,subscription,installplan,pods >&2 || true
  exit 1
}

# RHBK >= v24 uses Hostname v2: spec.hostname.hostname / .admin must be full
# URLs. deploy-rhbk.sh (written for v22) sets bare hostnames, which makes
# keycloak-0 CrashLoop on the newer channel. This runs in the background while
# deploy-rhbk.sh creates + waits on the Keycloak CR: as soon as the CR shows a
# bare hostname, rewrite it to https:// and delete keycloak-0 so the StatefulSet
# rolls a pod with the fixed config. Exits on its own; a no-op if the value is
# already a URL. --crc only.
crc_fix_keycloak_hostname_v2() {
  local ns="$1" end=$((SECONDS + 600)) h
  while (( SECONDS < end )); do
    h="$("$KUBECTL" -n "$ns" get keycloak keycloak -o jsonpath='{.spec.hostname.hostname}' 2>/dev/null || true)"
    if [[ -n "$h" ]]; then
      case "$h" in
        http://*|https://*) return 0 ;;
        *)
          echo "  [crc] rewriting Keycloak hostname '${h}' -> 'https://${h}' (RHBK Hostname v2)"
          "$KUBECTL" -n "$ns" patch keycloak keycloak --type=merge \
            -p "{\"spec\":{\"hostname\":{\"hostname\":\"https://${h}\",\"admin\":\"https://${h}\"}}}" >/dev/null 2>&1 || true
          "$KUBECTL" -n "$ns" delete pod keycloak-0 --ignore-not-found >/dev/null 2>&1 || true
          return 0 ;;
      esac
    fi
    sleep 5
  done
}

step "BYOI dependencies (Kafka, Postgres, Valkey, MinIO, Keycloak)"
if [[ "$DEMO_CRC" == "1" ]] && ! keycloak_ready; then
  ensure_rhbk_channel "$KEYCLOAK_NAMESPACE" "${DEMO_RHBK_CHANNEL:-stable-v26}"
fi
SKIP_KAFKA=0 SKIP_INFRA=0 SKIP_KEYCLOAK=0 SKIP_OAUTH_MIRROR=0
kafka_ready && SKIP_KAFKA=1 && skip "Kafka ${KAFKA_CLUSTER_NAME} already Ready"
infra_ready && SKIP_INFRA=1 && skip "infra in ${INFRA_NAMESPACE} already rolled out"
keycloak_ready && SKIP_KEYCLOAK=1 && skip "Keycloak already Ready"
oauth_secret_ready && SKIP_OAUTH_MIRROR=1 && skip "OAuth client Secret already present"
if [[ "${SKIP_KAFKA}${SKIP_INFRA}${SKIP_KEYCLOAK}${SKIP_OAUTH_MIRROR}" == "1111" ]]; then
  skip "all BYOI stages healthy"
else
  export SKIP_KAFKA SKIP_INFRA SKIP_KEYCLOAK SKIP_OAUTH_MIRROR
  KC_FIX_PID=""
  if [[ "$DEMO_CRC" == "1" && "$SKIP_KEYCLOAK" != "1" ]]; then
    crc_fix_keycloak_hostname_v2 "$KEYCLOAK_NAMESPACE" &
    KC_FIX_PID=$!
  fi
  ./hack/deploy-byoi.sh
  [[ -n "$KC_FIX_PID" ]] && wait "$KC_FIX_PID" 2>/dev/null || true
fi
ok "BYOI ready"

step "Operator image"
BUILT_IMAGE=0
docker_up() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1
}
build_openshift() {
  echo "  OpenShift binary docker build -> ${NAMESPACE}/koku-service-operator:${IMAGE_TAG}"
  if ! "$KUBECTL" -n "$NAMESPACE" get buildconfig koku-service-operator >/dev/null 2>&1; then
    oc -n "$NAMESPACE" new-build --name=koku-service-operator --binary --strategy=docker \
      --to="koku-service-operator:${IMAGE_TAG}"
  fi
  local tmp
  tmp="$(mktemp -d)"
  tar -C "$ROOT" -cf - \
    --exclude .git --exclude .serena --exclude bin --exclude .worktrees \
    --exclude docs/superpowers --exclude '*.out' \
    . | tar -C "$tmp" -xf -
  patch_operator_dockerfile "${tmp}/Dockerfile"
  oc -n "$NAMESPACE" start-build koku-service-operator --from-dir="$tmp" --follow
  rm -rf "$tmp"
}
build_docker() {
  echo "  docker buildx --platform linux/amd64 -t ${IMG}"
  docker buildx build --platform linux/amd64 -t "$IMG" --push "$ROOT"
}

if [[ "$DEMO_REBUILD" != "1" ]] && image_present && operator_ready; then
  skip "ImageStream tag and operator Deployment already present (--rebuild to force)"
else
  mode="$BUILD_MODE"
  if [[ "$mode" == "auto" ]]; then
    case "$IMG" in
      image-registry.openshift-image-registry.svc:*)
        mode=openshift
        ;;
      *)
        if docker_up; then
          mode=docker
        else
          mode=openshift
        fi
        ;;
    esac
  fi
  case "$mode" in
    docker)
      build_docker
      BUILT_IMAGE=1
      ;;
    openshift)
      build_openshift
      BUILT_IMAGE=1
      ;;
    *)
      echo "error: BUILD_MODE must be auto|docker|openshift (got ${mode})" >&2
      exit 1
      ;;
  esac
  ok "image ${IMG}"
fi

step "In-cluster operator"
if [[ "$DEMO_REBUILD" != "1" && "$BUILT_IMAGE" != "1" ]] && operator_ready; then
  skip "koku-service-operator Deployment already Available"
else
  IMG="$IMG" ./hack/deploy-incluster.sh "$NAMESPACE"
fi
ok "operator running"

step "Apply CostManagementServiceConfig"
CR_TMP="$(mktemp)"
render_preprod_cr \
  "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" \
  "$CR_TMP" \
  "$DOMAIN" \
  "$KEYCLOAK_ISSUER"
"$KUBECTL" apply -f "$CR_TMP"
rm -f "$CR_TMP"
ok "CR ${NAMESPACE}/${CR_NAME} applied (issuer ${KEYCLOAK_ISSUER})"

step "Wait for UI Deployment"
echo "  watching ${NAMESPACE}/${CR_NAME} (UIReady is the OAuth Secret; we wait for the UI rollout)…"
deadline=$((SECONDS + 900))
while (( SECONDS < deadline )); do
  "$KUBECTL" -n "$NAMESPACE" get cmsc "$CR_NAME" -o jsonpath='phase={.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}' 2>/dev/null || true
  if ui_deploy_ready; then
    "$KUBECTL" -n "$NAMESPACE" rollout status "deploy/${CR_NAME}-ui" --timeout=120s
    break
  fi
  sleep 15
done
if ! ui_deploy_ready; then
  echo "error: UI Deployment ${CR_NAME}-ui did not become Available within 15m" >&2
  "$KUBECTL" -n "$NAMESPACE" get cmsc "$CR_NAME" -o yaml | tail -80 >&2 || true
  exit 1
fi
ok "UI Deployment Ready"

step "Open the UI"
echo "  ${UI_URL}"
echo "  Keycloak realm user: admin / admin"
if [[ "$DEMO_NO_OPEN" == "1" || "$OPEN_BROWSER" == "0" ]]; then
  skip "browser launch disabled"
elif command -v open >/dev/null 2>&1; then
  open "$UI_URL" || true
  ok "opened in default browser (cluster TLS may need a click-through)"
else
  skip "no 'open' command; visit the URL above"
fi

echo ""
echo "${C_BOLD}${C_GREEN}Demo complete.${C_RESET} UI: ${UI_URL}"
echo "Login: admin / admin"
