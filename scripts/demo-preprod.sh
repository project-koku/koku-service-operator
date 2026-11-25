#!/usr/bin/env bash
# Full pre-prod demo: prerequisites (AMQ Streams Kafka, PostgreSQL, Valkey,
# MinIO-compatible object storage, RHBK Keycloak) → koku-service-operator →
# Cost Management UI.
#
# The prerequisites are "BYOI" (bring your own infrastructure): the operator
# never provisions them — it validates and connects. This demo installs
# disposable stand-ins so you can evaluate without real infrastructure.
#
# Usage (from repo root or scripts/):
#   ./scripts/demo-preprod.sh
#   ./scripts/demo-preprod.sh --dry-run
#   ./scripts/demo-preprod.sh --reset          # delete app/infra/kafka/keycloak NS first
#   ./scripts/demo-preprod.sh --no-tmux
#   ./scripts/demo-preprod.sh --rebuild        # force operator image rebuild
#   ./scripts/demo-preprod.sh --no-open        # never open the UI in a browser
#
# Operator install source (--source=registry|local|manual; default: registry):
#   registry  install the published operator (quay.io/project-koku) via OLM
#   local     build the operator from this checkout, deploy in-cluster
#   manual    prepare prereqs + CR; you install the operator via the console
#
# Version selection (registry and manual sources):
#   --version <vX.Y.Z|commit-sha|latest>   default: latest stable (discovered)
#   --catalog-repo <quay namespace>        default: quay.io/project-koku
#   --channel <olm channel>                default: beta
#
#   ./scripts/demo-preprod.sh --watch         # observe an existing install until the UI is ready
#   ./scripts/demo-preprod.sh --console       # print the OpenShift console URL and username
#   ./scripts/demo-preprod.sh --prereqs-only  # install prerequisites only, then stop
#   ./scripts/demo-preprod.sh --skip-prereqs --cr-file <cmsc.yaml>  # operator on existing infra
#   ./scripts/demo-preprod.sh --profile=single-node   # small-cluster Kafka sizing (alias: --crc)
#
# Settings: env vars, then scripts/demo-preprod.local.env, then
# scripts/demo-preprod.env.example.
#
# tmux (default, when installed): left pane = numbered steps; top-right =
# kubectl klock pods in NAMESPACE; bottom-right = kubectl klock pods in
# KAFKA_NAMESPACE. Disable with --no-tmux; detach with Ctrl-b d.
#
# HELP-END — usage output ends at this marker.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
SCRIPT="${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")"
cd "$ROOT"
# shellcheck disable=SC1091
source "${ROOT}/scripts/lib/demo-preprod.bash"

# Raw invocation, captured before flag parsing: the tmux wrapper re-invokes
# the script with these so every flag reaches the inner run (criticals #13).
ORIGINAL_ARGS=("$@")

DEMO_RESET="${DEMO_RESET:-0}"
DEMO_DRY_RUN="${DEMO_DRY_RUN:-0}"
DEMO_NO_TMUX="${DEMO_NO_TMUX:-0}"
DEMO_REBUILD="${DEMO_REBUILD:-0}"
DEMO_NO_OPEN="${DEMO_NO_OPEN:-0}"
PROFILE="${PROFILE:-}"
DEMO_WATCH="${DEMO_WATCH:-0}"
DEMO_CONSOLE="${DEMO_CONSOLE:-0}"
PREREQS_ONLY="${PREREQS_ONLY:-0}"
SKIP_PREREQS="${SKIP_PREREQS:-0}"
USER_CR_FILE="${USER_CR_FILE:-}"

usage() {
  sed -n '2,/^# HELP-END/p' "$SCRIPT" | sed 's/^# \{0,1\}//' | sed '/^HELP-END/d'
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --reset) DEMO_RESET=1 ;;
    --dry-run) DEMO_DRY_RUN=1 ;;
    --no-tmux) DEMO_NO_TMUX=1 ;;
    --rebuild) DEMO_REBUILD=1 ;;
    --no-open) DEMO_NO_OPEN=1 ;;
    --crc) PROFILE="single-node" ;;
    --profile)
      [[ $# -ge 2 ]] || { echo "error: --profile requires a value (single-node)" >&2; exit 2; }
      PROFILE="$2"
      shift
      ;;
    --profile=*)
      PROFILE="${1#--profile=}"
      ;;
    --watch) DEMO_WATCH=1 ;;
    --console) DEMO_CONSOLE=1 ;;
    --prereqs-only) PREREQS_ONLY=1 ;;
    --skip-prereqs) SKIP_PREREQS=1 ;;
    --cr-file)
      [[ $# -ge 2 ]] || { echo "error: --cr-file requires a path" >&2; exit 2; }
      USER_CR_FILE="$2"
      shift
      ;;
    --source)
      [[ $# -ge 2 ]] || { echo "error: --source requires a value (registry|local|manual)" >&2; exit 2; }
      SOURCE="$2"
      shift
      ;;
    --version)
      [[ $# -ge 2 ]] || { echo "error: --version requires a value (vX.Y.Z, commit SHA, latest)" >&2; exit 2; }
      VERSION="$2"
      shift
      ;;
    --catalog-repo)
      [[ $# -ge 2 ]] || { echo "error: --catalog-repo requires a value (quay namespace)" >&2; exit 2; }
      CATALOG_REPO="$2"
      shift
      ;;
    --channel)
      [[ $# -ge 2 ]] || { echo "error: --channel requires a value" >&2; exit 2; }
      CHANNEL="$2"
      shift
      ;;
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

# load_env_file only sets vars that are still unset (first file wins), so the
# load order below is the precedence:
#   real env > .local.env > .env.example
# (.local.env must win over the example's uncommented defaults — review
# critical #14. arm64 workload image overrides live in
# scripts/demo-preprod.arm64.env — an opt-in template you copy into
# .local.env; no flag auto-loads it.)
load_env_file "${ROOT}/scripts/demo-preprod.local.env"
load_env_file "${ROOT}/scripts/demo-preprod.env.example"

# Default: the current kube context. Pin one with KUBE_CONTEXT=... when
# juggling several clusters.
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
SOURCE="${SOURCE:-registry}"
case "$SOURCE" in
  registry|local|manual) ;;
  *)
    echo "error: SOURCE must be registry, local or manual (got '${SOURCE}')" >&2
    usage >&2
    exit 2
    ;;
esac
if [[ ( -n "${REGISTRY_TAG_FIXTURE:-}" || -n "${IMAGE_ARCH_FIXTURE:-}" ) && "${DEMO_ALLOW_FIXTURES:-}" != "1" ]]; then
  echo "error: a fixture variable (REGISTRY_TAG_FIXTURE/IMAGE_ARCH_FIXTURE) is set but fixture" >&2
  echo "  mode is not enabled. Fixture hooks are for the offline test suite only; aborting" >&2
  echo "  rather than installing from faked version/architecture data." >&2
  exit 1
fi
if [[ "$PREREQS_ONLY" == "1" && "$SKIP_PREREQS" == "1" ]]; then
  echo "error: --prereqs-only and --skip-prereqs are mutually exclusive" >&2
  exit 2
fi
if [[ -n "$USER_CR_FILE" && ! -f "$USER_CR_FILE" ]]; then
  echo "error: --cr-file not found: ${USER_CR_FILE}" >&2
  exit 2
fi
# The user CR must land where the script expects (cross-model review #2): a
# CR with different metadata.name/namespace applied elsewhere produced a false
# "applied" and a 15-minute wait in the wrong namespace.
if [[ -n "$USER_CR_FILE" ]]; then
  read -r CR_FILE_NAME CR_FILE_NS <<<"$(python3 - "$USER_CR_FILE" <<'PY2'
import re, sys
text = open(sys.argv[1]).read()
name = re.search(r"^\s*name:\s*(\S+)", text, re.M)
ns = re.search(r"^\s*namespace:\s*(\S+)", text, re.M)
print(f"{name.group(1) if name else '-'} {ns.group(1) if ns else '-'}")
PY2
)" || true
  if [[ "$CR_FILE_NAME" != "$CR_NAME" || ( -n "$CR_FILE_NS" && "$CR_FILE_NS" != "$NAMESPACE" ) ]]; then
    echo "error: --cr-file metadata (name: ${CR_FILE_NAME}, namespace: ${CR_FILE_NS:-<none>}) does not" >&2
    echo "  match the script's target (name: ${CR_NAME}, namespace: ${NAMESPACE}). Fix the CR or set" >&2
    echo "  CR_NAME/NAMESPACE to match." >&2
    exit 2
  fi
fi
validate_version_hint "${VERSION:-}" || exit 2
if [[ -n "${CATALOG_IMG:-}" && ! "${CATALOG_IMG:-}" =~ ^[A-Za-z0-9._/:@-]+$ ]]; then
  echo "error: CATALOG_IMG has invalid characters (allowed: A-Za-z0-9._/:@-): ${CATALOG_IMG}" >&2
  exit 2
fi
# Everything interpolated into the cluster-admin OLM apply (cross-model review
# #6): channel and names must be plain identifiers.
if [[ -n "${CHANNEL:-}" && ! "${CHANNEL:-}" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo "error: CHANNEL must match [A-Za-z0-9._-]+ (got '${CHANNEL}')" >&2
  exit 2
fi
for _nsvar in NAMESPACE CR_NAME INFRA_NAMESPACE KAFKA_NAMESPACE KEYCLOAK_NAMESPACE CATALOG_NAME; do
  if [[ -n "${!_nsvar:-}" && ! "${!_nsvar}" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]; then
    echo "error: ${_nsvar} must be a DNS-1123 label (lowercase alphanumerics and '-'): ${!_nsvar}" >&2
    exit 2
  fi
done
case "$PROFILE" in
  ""|single-node) ;;
  *)
    echo "error: PROFILE must be single-node (got '${PROFILE}')" >&2
    exit 2
    ;;
esac
if [[ "$PROFILE" == "single-node" ]]; then
  # Single-node / small-cluster sizing — nothing else. Architecture is
  # autodiscovered; KUBE_CONTEXT and BUILD_MODE are never forced; arm64 image
  # overrides are the opt-in demo-preprod.arm64.env template.
  KAFKA_BROKER_REPLICAS="${KAFKA_BROKER_REPLICAS:-1}"
  KAFKA_CONTROLLER_REPLICAS="${KAFKA_CONTROLLER_REPLICAS:-1}"
  KAFKA_BROKER_STORAGE="${KAFKA_BROKER_STORAGE:-10Gi}"
  KAFKA_CONTROLLER_STORAGE="${KAFKA_CONTROLLER_STORAGE:-5Gi}"
  export KAFKA_BROKER_REPLICAS KAFKA_CONTROLLER_REPLICAS KAFKA_BROKER_STORAGE KAFKA_CONTROLLER_STORAGE
fi
NAMESPACE="${NAMESPACE:-cost-byoi}"
CR_NAME="${CR_NAME:-cost-management}"
INFRA_NAMESPACE="${INFRA_NAMESPACE:-cost-byoi-infra}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
KAFKA_CLUSTER_NAME="${KAFKA_CLUSTER_NAME:-cost-onprem-kafka}"
CATALOG_REPO="${CATALOG_REPO:-quay.io/project-koku}"
# A bare namespace would build a docker.io image reference while discovery
# still queries quay.io (cross-model review #7) — normalize to a full host.
case "$CATALOG_REPO" in
  */*) : ;;
  *) CATALOG_REPO="quay.io/${CATALOG_REPO}" ;;
esac
CATALOG_NAME="${CATALOG_NAME:-koku-service-operator-catalog}"
VERSION="${VERSION:-}"
CHANNEL="${CHANNEL:-beta}"
ARCH_CHECK="${ARCH_CHECK:-1}"
CONSOLE_USERNAME="${CONSOLE_USERNAME:-kubeadmin}"
CONSOLE_PASSWORD="${CONSOLE_PASSWORD:-}"
CR_FILE="${CR_FILE:-/tmp/koku-demo-preprod-${NAMESPACE}.yaml}"

# Resolve the catalog image before printing the plan so --dry-run shows the
# real image. CATALOG_IMG set explicitly skips discovery entirely, and
# --watch/--console skip it too (they never use the catalog — no network I/O
# before the mode dispatch; review note).
if [[ -z "${CATALOG_IMG:-}" && ( "$DEMO_WATCH" == "1" || "$DEMO_CONSOLE" == "1" || "$SOURCE" == "local" ) ]]; then
  CATALOG_IMG=""
  CATALOG_VERSION="(not used)"
elif [[ -z "${CATALOG_IMG:-}" ]]; then
  REPO_PATH="${CATALOG_REPO#quay.io/}"
  REPO_PATH="${REPO_PATH%/}"
  if RESOLVED_VERSION="$(resolve_catalog_version "${REPO_PATH}/koku-service-operator-catalog" "${VERSION:-}")"; then
    CATALOG_IMG="${CATALOG_REPO}/koku-service-operator-catalog:${RESOLVED_VERSION}"
    CATALOG_VERSION="$RESOLVED_VERSION"
  else
    exit 1
  fi
else
  CATALOG_VERSION="${CATALOG_IMG##*/}"
  CATALOG_VERSION="${CATALOG_VERSION#*:}"
fi
export CATALOG_IMG
OPEN_BROWSER="${OPEN_BROWSER:-1}"
BUILD_MODE="${BUILD_MODE:-auto}"
IMAGE_TAG="${IMAGE_TAG:-preprod}"
TMUX_SESSION="${TMUX_SESSION:-koku-demo-preprod}"
WATCHER="${WATCHER:-klock}"
LOG_LEVEL="${LOG_LEVEL:-INFO}"
KUBECTL="${KUBECTL:-oc}"

RHBK_SCRIPT="${RHBK_SCRIPT:-${ROOT}/scripts/deploy-rhbk.sh}"
export RHBK_SCRIPT NAMESPACE CR_NAME INFRA_NAMESPACE KAFKA_NAMESPACE KEYCLOAK_NAMESPACE LOG_LEVEL SOURCE

IMG="${IMG:-image-registry.openshift-image-registry.svc:5000/${NAMESPACE}/koku-service-operator:${IMAGE_TAG}}"
export IMG

STEP_N=0
# The step count and plan below must match what the selected source runs
# (review warning #20): registry has no build steps; manual stops at the
# instructions.
# Flag-parsed values must be exported: the tmux wrapper serializes the
# exported environment into the inner run (a pane inherits the tmux SERVER's
# environment, not the caller's, so shell variables would be lost).
export DEMO_RESET DEMO_REBUILD DEMO_NO_OPEN PROFILE PREREQS_ONLY SKIP_PREREQS \
       USER_CR_FILE VERSION CHANNEL CATALOG_REPO ARCH_CHECK CONTAINER_TOOL \
       KUBE_CONTEXT OPEN_BROWSER CONSOLE_USERNAME CONSOLE_PASSWORD CR_FILE \
       BUILD_MODE IMAGE_TAG IMG WATCHER STORAGE_CLASS DEMO_SMOKE DEMO_RHBK_CHANNEL
case "$SOURCE" in
  registry) STEP_TOTAL=7 ;;
  local) STEP_TOTAL=8 ;;
  manual) STEP_TOTAL=4 ;;
esac

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
# skip_step: a skip that REPLACES an entire plan step (only --reset's).
skip_step() {
  STEP_N=$((STEP_N + 1))
  echo "${C_YELLOW}  -> [${STEP_N}/${STEP_TOTAL}] skip: $*${C_RESET}"
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
  context:     ${KUBE_CONTEXT:-<current>}
  source:      ${SOURCE}
  catalog:     ${CATALOG_IMG:-<not resolved>}
  version:     ${CATALOG_VERSION}
  app NS:      ${NAMESPACE}  (CR ${CR_NAME})
  infra NS:    ${INFRA_NAMESPACE}
  kafka NS:    ${KAFKA_NAMESPACE}
  keycloak NS: ${KEYCLOAK_NAMESPACE}
  RHBK script: ${RHBK_SCRIPT}
  IMG:         ${IMG}
  BUILD_MODE:  ${BUILD_MODE}
  watcher:     ${WATCHER}
  tmux:        ${TMUX_SESSION}  (NO_TMUX=${DEMO_NO_TMUX})
  reset:       ${DEMO_RESET}
  rebuild:     ${DEMO_REBUILD}
  profile:     ${PROFILE:-default}$([[ "$PROFILE" == "single-node" ]] && echo "  (Kafka 1 broker + 1 controller, 10Gi/5Gi)")
  open UI:     ${OPEN_BROWSER} (NO_OPEN=${DEMO_NO_OPEN})

  [1/${STEP_TOTAL}] Preflight (context, oc, RHBK script, StorageClass)
  [2/${STEP_TOTAL}] --reset: delete ${NAMESPACE}, ${INFRA_NAMESPACE}, ${KAFKA_NAMESPACE}, ${KEYCLOAK_NAMESPACE}
  [3/${STEP_TOTAL}] BYOI (./scripts/deploy-byoi.sh) — skip inner stages that are already Ready
EOF
  case "$SOURCE" in
    registry)
      cat <<EOF
  [4/${STEP_TOTAL}] Operator install (registry via OLM: CatalogSource, OperatorGroup, Subscription)
  [5/${STEP_TOTAL}] Apply CR (clusterDomain + Keycloak issuerURL + insecureSkipVerify, ROS off)
  [6/${STEP_TOTAL}] Wait for UI Deployment + CMSC verification table
  [7/${STEP_TOTAL}] open the UI Route
EOF
      ;;
    local)
      cat <<EOF
  [4/${STEP_TOTAL}] Operator image (${BUILD_MODE}: docker or OpenShift binary build)
  [5/${STEP_TOTAL}] In-cluster operator (./scripts/deploy-incluster.sh)
  [6/${STEP_TOTAL}] Apply CR (clusterDomain + Keycloak issuerURL + insecureSkipVerify, ROS off)
  [7/${STEP_TOTAL}] Wait for UI Deployment + CMSC verification table
  [8/${STEP_TOTAL}] open the UI Route
EOF
      ;;
    manual)
      cat <<EOF
  [4/${STEP_TOTAL}] Render CR + console install instructions (then --watch)
EOF
      ;;
  esac

  if [[ "$PREREQS_ONLY" == "1" ]]; then
    echo "  Prereqs only: stopping after the prerequisites."
  fi
  if [[ "$SKIP_PREREQS" == "1" ]]; then
    echo "  Skip prereqs: BYOI is skipped; the operator installs on existing infrastructure."
  fi
  if [[ -n "$USER_CR_FILE" ]]; then
    echo "  CR:          ${USER_CR_FILE} (user-provided; rendering skipped)"
  fi
  if [[ "$SOURCE" == "manual" ]]; then
    cat <<EOF
  CR file:     ${CR_FILE}

  Manual source: after the prerequisites, the CMSC is rendered to the CR file.
  Install the operator through the OpenShift console (instructions printed at
  the end of the run), then observe with: ./scripts/demo-preprod.sh --watch
EOF
  fi
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

  # The inner run must see the CALLER's effective configuration — flags, env
  # vars, and env-file settings alike. A tmux pane inherits the SERVER's
  # environment (not the caller's), so shell exports do not carry over when a
  # server is already running, and a forwarded allowlist cannot scale to every
  # configuration variable (cross-model review: ~25 documented variables were
  # still dropped). Instead, serialize the whole exported environment plus the
  # raw invocation into a runner script the pane executes.
  local runner
  runner="$(mktemp "${TMPDIR:-/tmp}/demo-preprod-runner.XXXXXX")"
  chmod 600 "$runner"
  {
    declare -x -p
    printf 'rm -f %q\n' "$runner"
    printf 'exec %q' "$ROOT/scripts/demo-preprod.sh"
    if [[ ${#ORIGINAL_ARGS[@]} -gt 0 ]]; then
      printf ' %q' "${ORIGINAL_ARGS[@]}"
    fi
    printf '\n'
  } >"$runner"
  inner="cd $(printf %q "$ROOT") && DEMO_INNER=1 bash $(printf %q "$runner")"
  tmux send-keys -t "${TMUX_SESSION}:demo.0" "$inner" C-m
  tmux select-pane -t "${TMUX_SESSION}:demo.0"

  if [[ -n "${TMUX:-}" ]]; then
    tmux switch-client -t "$TMUX_SESSION"
  else
    tmux attach-session -t "$TMUX_SESSION"
  fi
}

if [[ -z "${DEMO_INNER:-}" && "$DEMO_NO_TMUX" != "1" && "$DEMO_WATCH" != "1" && "$DEMO_CONSOLE" != "1" ]] && command -v tmux >/dev/null 2>&1; then
  start_tmux
  exit 0
fi

# --- inner / no-tmux path ----------------------------------------------------
need_cmd "$KUBECTL"
need_cmd python3
need_cmd openssl

demo_profile_banner

step "Preflight"
if [[ -n "${KUBE_CONTEXT}" ]]; then
  echo "Cluster context: ${KUBE_CONTEXT}"
  if ! "$KUBECTL" config get-contexts "$KUBE_CONTEXT" >/dev/null 2>&1; then
    echo "error: kube context '${KUBE_CONTEXT}' not found" >&2
    "$KUBECTL" config get-contexts >&2 || true
    exit 1
  fi
  "$KUBECTL" config use-context "$KUBE_CONTEXT" >/dev/null
else
  echo "Cluster context: <current>"
fi
require_reachable_cluster "$KUBECTL" || exit 1
ok "using $($KUBECTL config current-context) — $($KUBECTL whoami --show-server)"

# The registry source needs cluster-admin (CatalogSource in
# openshift-marketplace): check BEFORE the prerequisites install, not after
# (cross-model review #3 — a restricted user otherwise waits tens of minutes
# for the mid-run failure the docs promise to avoid).
if [[ "$SOURCE" == "registry" ]]; then
  if ! "$KUBECTL" auth can-i create catalogsources -n openshift-marketplace >/dev/null 2>&1; then
    echo "error: the registry source needs cluster-admin: it creates a CatalogSource" >&2
    echo "  in openshift-marketplace. Use --source=manual and have your cluster" >&2
    echo "  administrator install the operator, or run on a cluster where you have" >&2
    echo "  cluster-admin." >&2
    exit 1
  fi
fi

# Customer-friendly early failure: the demo namespaces must exist or be
# creatable (a developer sandbox or restricted user cannot create them). With
# --skip-prereqs only the application namespace matters.
if [[ "$SKIP_PREREQS" == "1" ]]; then
  _ns_list=("$NAMESPACE")
else
  _ns_list=("$NAMESPACE" "$INFRA_NAMESPACE" "$KAFKA_NAMESPACE" "$KEYCLOAK_NAMESPACE")
fi
for _ns in "${_ns_list[@]}"; do
  if ! "$KUBECTL" get ns "$_ns" >/dev/null 2>&1; then
    if ! "$KUBECTL" auth can-i create namespaces >/dev/null 2>&1; then
      echo "error: namespace '${_ns}' does not exist and this user cannot create namespaces." >&2
      echo "  The demo needs four namespaces (${NAMESPACE}, ${INFRA_NAMESPACE}, ${KAFKA_NAMESPACE}, ${KEYCLOAK_NAMESPACE})." >&2
      echo "  Ask your cluster administrator, or point NAMESPACE/INFRA_NAMESPACE/" >&2
      echo "  KAFKA_NAMESPACE/KEYCLOAK_NAMESPACE at namespaces you can already use." >&2
      exit 1
    fi
  fi
done

if [[ ! -x "$RHBK_SCRIPT" ]]; then
  echo "error: RHBK script not found or not executable: ${RHBK_SCRIPT}" >&2
  echo "  set RHBK_SCRIPT to an executable deploy-rhbk.sh" >&2
  exit 1
fi
ok "RHBK script=${RHBK_SCRIPT}"

DOMAIN="$(read_apps_domain "$KUBECTL")" || exit 1
ok "apps domain=${DOMAIN}"
KEYCLOAK_ISSUER="https://keycloak-${KEYCLOAK_NAMESPACE}.${DOMAIN}"
UI_URL="https://${CR_NAME}-ui-${NAMESPACE}.${DOMAIN}"
CONSOLE_URL="https://console-openshift-console.${DOMAIN}"

print_console_access() {
  echo "OpenShift Console: ${CONSOLE_URL}"
  echo "Console username: ${CONSOLE_USERNAME}"
  if [[ -n "$CONSOLE_PASSWORD" ]]; then
    echo "Console password: ${CONSOLE_PASSWORD}"
  else
    echo "Console password: set CONSOLE_PASSWORD, or retrieve it from your cluster's"
    echo "  provisioning output (kubeadmin; CRC prints it via 'crc console --credentials')."
  fi
}

# wait_for_ui lives in scripts/lib (parameterized, credential-free); the
# call sites below print login details after it succeeds.

start_watch_tmux() {
  need_cmd tmux
  tmux has-session -t "$TMUX_SESSION" 2>/dev/null && tmux kill-session -t "$TMUX_SESSION"
  tmux new-session -d -s "$TMUX_SESSION" -n demo
  tmux send-keys -t "${TMUX_SESSION}:demo.0" \
    "$KUBECTL -n $(printf %q "$NAMESPACE") get cmsc $(printf %q "$CR_NAME") -w" C-m
  tmux split-window -h -t "${TMUX_SESSION}:demo" \
    "$KUBECTL -n $(printf %q "$NAMESPACE") get pods -w"
  local runner
  runner="$(mktemp "${TMPDIR:-/tmp}/demo-preprod-runner.XXXXXX")"
  chmod 600 "$runner"
  {
    declare -x -p
    printf 'rm -f %q\n' "$runner"
    printf 'exec %q' "$SCRIPT"
    if [[ ${#ORIGINAL_ARGS[@]} -gt 0 ]]; then
      printf ' %q' "${ORIGINAL_ARGS[@]}"
    fi
    printf '\n'
  } >"$runner"
  tmux split-window -v -t "${TMUX_SESSION}:demo.1" \
    "cd $(printf %q "$ROOT") && DEMO_INNER=1 DEMO_NO_TMUX=1 bash $(printf %q "$runner")"
  tmux select-pane -t "${TMUX_SESSION}:demo.0"
  if [[ -n "${TMUX:-}" ]]; then
    tmux switch-client -t "$TMUX_SESSION"
  else
    tmux attach-session -t "$TMUX_SESSION"
  fi
}

# Realm-user password read-back for the summary: runs BEFORE the watch
# dispatch so --watch prints the real password (a fresh install's Secret
# exists by then); on a first run the Secret does not exist yet and the
# BYOI step generates and stores one. An existing Secret that cannot be
# read (or reads empty) is an error, never a silent admin guess.
if [[ -z "${REALM_ADMIN_PASSWORD:-}" ]]; then
  if "$KUBECTL" -n "$NAMESPACE" get secret "${CR_NAME}-realm-admin" >/dev/null 2>&1; then
    # The Secret exists: a failed read is an error, never a silent admin guess.
    REALM_ADMIN_PASSWORD="$("$KUBECTL" -n "$NAMESPACE" get secret "${CR_NAME}-realm-admin" -o jsonpath='{.data.password}')" || {
      echo "error: cannot read Secret ${NAMESPACE}/${CR_NAME}-realm-admin" >&2
      exit 1
    }
    REALM_ADMIN_PASSWORD="$(printf '%s' "$REALM_ADMIN_PASSWORD" | base64 -d 2>/dev/null)" || true
    if [[ -z "$REALM_ADMIN_PASSWORD" ]]; then
      echo "error: Secret ${NAMESPACE}/${CR_NAME}-realm-admin has an empty password" >&2
      exit 1
    fi
  fi
  # No Secret: legacy pre-hardening install — the summary notes the legacy default.
fi

if [[ "$DEMO_CONSOLE" == "1" ]]; then
  print_console_access
  exit 0
fi

if [[ "$DEMO_WATCH" == "1" ]]; then
  if ! "$KUBECTL" -n "$NAMESPACE" get cmsc "$CR_NAME" >/dev/null 2>&1; then
    echo "error: CMSC ${NAMESPACE}/${CR_NAME} is not present." >&2
    echo "  Run --source=manual (or registry/local) first; --watch observes an existing install." >&2
    exit 1
  fi
  if [[ "$DEMO_NO_TMUX" == "1" ]]; then
    # Plain call: `wait_for_ui || exit 1` would disable errexit inside the
    # function (review critical #15) and report success after a failed rollout.
    wait_for_ui "$KUBECTL" "$NAMESPACE" "$CR_NAME" "$UI_URL" "$(should_open_browser)"
    echo "UI login: admin / ${REALM_ADMIN_PASSWORD:-admin (legacy default)}"
    echo "Retrieve later: ${KUBECTL} -n ${NAMESPACE} extract secret/${CR_NAME}-realm-admin --to=-"
    verify_conditions "$KUBECTL" "$NAMESPACE" "$CR_NAME" || {
      echo "error: CMSC verification failed — see the table above" >&2
      exit 1
    }
  else
    start_watch_tmux
  fi
  exit 0
fi

# Cluster architecture: gates the image-arch preflight and selects the
# local-build platform. Unsupported architectures fail here regardless of
# ARCH_CHECK (they cannot run the stack at all).
NODE_ARCH="$(cluster_node_arch "$KUBECTL")" || exit 1
ok "cluster architecture ${NODE_ARCH}"

# Render the CMSC once, early (the arch preflight needs its workload images,
# and the apply step reuses it) — unless the user supplied their own CR, in
# which case that file is the single source for both.
if [[ -n "$USER_CR_FILE" ]]; then
  CR_TO_APPLY="$USER_CR_FILE"
else
  CR_RENDER_TMP="$(mktemp)"
trap 'rm -f "${CR_RENDER_TMP:-}"' EXIT
  render_preprod_cr \
    "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" \
    "$CR_RENDER_TMP" \
    "$DOMAIN" \
    "$KEYCLOAK_ISSUER" \
    "$NAMESPACE" \
    "$CR_NAME"
  CR_TO_APPLY="$CR_RENDER_TMP"
fi

# Image-arch preflight: the catalog image (registry/manual sources) plus every
# workload image of the rendered CR. The operator image inside the CSV is
# checked after the OLM install (it is not knowable beforehand without opm).
if [[ "$ARCH_CHECK" == "1" ]]; then
  arch_images=()
  if [[ "$SOURCE" != "local" ]]; then
    arch_images+=("$CATALOG_IMG")
  fi
  while IFS= read -r _img; do
    [[ -n "$_img" ]] && arch_images+=("$_img")
  done < <(cr_workload_images "$CR_TO_APPLY")
  assert_arch_compatible "$NODE_ARCH" ${arch_images[@]+"${arch_images[@]}"} || exit 1
  ok "image architectures compatible with ${NODE_ARCH}"
else
  echo "warning: ARCH_CHECK=0 — image architecture checks skipped" >&2
fi

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
  deploy_available "$KUBECTL" "$NAMESPACE" koku-service-operator
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
  # OLM objects go after the CMSC deletion (the operator cleared its finalizer
  # above) and before the namespaces: Subscription, CSV, OperatorGroup in the
  # app namespace, plus our CatalogSource in openshift-marketplace.
  reset_olm_objects "$KUBECTL" "$NAMESPACE" "$CATALOG_NAME"

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
# already exists — that is expected and harmless. With --skip-prereqs only the
# application namespace is ours to create; the user's infrastructure lives
# wherever their CR points.
if [[ "$SKIP_PREREQS" == "1" ]]; then
  need_ns "$NAMESPACE"
else
  need_ns "$NAMESPACE"
  need_ns "$INFRA_NAMESPACE"
  need_ns "$KAFKA_NAMESPACE"
  need_ns "$KEYCLOAK_NAMESPACE"
fi

kafka_ready() {
  [[ "$("$KUBECTL" -n "$KAFKA_NAMESPACE" get kafka "$KAFKA_CLUSTER_NAME" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)" == "True" ]]
}
infra_ready() {
  # SKIP_INFRA skips Postgres AND Valkey AND MinIO AND the minio-init job —
  # all four must be healthy before skipping (cross-model review #4: one
  # surviving component green-lit a partially torn-down infra).
  deploy_available "$KUBECTL" "$INFRA_NAMESPACE" postgresql \
    && deploy_available "$KUBECTL" "$INFRA_NAMESPACE" valkey \
    && deploy_available "$KUBECTL" "$INFRA_NAMESPACE" minio \
    && [[ "$("$KUBECTL" -n "$INFRA_NAMESPACE" get job minio-init -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || true)" == "True" ]]
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

# RHBK / Keycloak operator: the channel deploy-rhbk.sh pins (stable-v22) ships
# amd64-only operator images. On arm64 clusters we pre-create the
# Subscription on an arm64-capable channel (stable-v26); deploy-rhbk.sh then
# finds it and skips its own create. No-op on amd64 clusters.
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
# already a URL. arm64 clusters only (they need the stable-v26 channel).
fix_keycloak_hostname_v2() {
  local ns="$1" end=$((SECONDS + 1200)) h
  while (( SECONDS < end )); do
    h="$("$KUBECTL" -n "$ns" get keycloak keycloak -o jsonpath='{.spec.hostname.hostname}' 2>/dev/null || true)"
    if [[ -n "$h" ]]; then
      case "$h" in
        http://*|https://*) return 0 ;;
        *)
          echo "  [crc] rewriting Keycloak hostname '${h}' -> 'https://${h}' (RHBK Hostname v2)"
          if ! "$KUBECTL" -n "$ns" patch keycloak keycloak --type=merge \
            -p "{\"spec\":{\"hostname\":{\"hostname\":\"https://${h}\",\"admin\":\"https://${h}\"}}}" >/dev/null 2>&1; then
            echo "  [crc] Keycloak hostname patch failed; retrying"
            sleep 5
            continue
          fi
          "$KUBECTL" -n "$ns" delete pod keycloak-0 --ignore-not-found >/dev/null 2>&1 || true
          return 0 ;;
      esac
    fi
    sleep 5
  done
  echo "  [crc] WARNING: timed out before Keycloak hostname was rewritten to a v2 URL" >&2
  return 1
}

if [[ "$SKIP_PREREQS" == "1" ]]; then
  skip_step "BYOI skipped (--skip-prereqs): the operator installs on existing infrastructure"
else
step "BYOI dependencies (Kafka, Postgres, Valkey, MinIO, Keycloak)"
if [[ "$NODE_ARCH" == "arm64" ]] && ! keycloak_ready; then
  ensure_rhbk_channel "$KEYCLOAK_NAMESPACE" "${DEMO_RHBK_CHANNEL:-stable-v26}"
fi
SKIP_KAFKA=0 SKIP_INFRA=0 SKIP_KEYCLOAK=0 SKIP_OAUTH_MIRROR=0
kafka_ready && SKIP_KAFKA=1 && skip "Kafka ${KAFKA_CLUSTER_NAME} already Ready"
infra_ready && SKIP_INFRA=1 && skip "infra in ${INFRA_NAMESPACE} already rolled out"
keycloak_ready && SKIP_KEYCLOAK=1 && skip "Keycloak already Ready"
oauth_secret_ready && SKIP_OAUTH_MIRROR=1 && skip "OAuth client Secret already present"
# Demo hardening: a fresh Keycloak provisioning gets a random realm-user
# password (stored in a Secret afterwards). IQE/CI set REALM_ADMIN_PASSWORD
# themselves or use DEMO_SMOKE; deploy-rhbk.sh's own default stays admin/admin.
if [[ "$SKIP_KEYCLOAK" != "1" && -z "${REALM_ADMIN_PASSWORD:-}" ]]; then
  REALM_ADMIN_PASSWORD="$(openssl rand -hex 12)"
  export REALM_ADMIN_PASSWORD
fi
if [[ "${SKIP_KAFKA}${SKIP_INFRA}${SKIP_KEYCLOAK}${SKIP_OAUTH_MIRROR}" == "1111" ]]; then
  skip "all BYOI stages healthy"
else
  export SKIP_KAFKA SKIP_INFRA SKIP_KEYCLOAK SKIP_OAUTH_MIRROR
  KC_FIX_PID=""
  if [[ "$NODE_ARCH" == "arm64" && "$SKIP_KEYCLOAK" != "1" ]]; then
    fix_keycloak_hostname_v2 "$KEYCLOAK_NAMESPACE" &
    KC_FIX_PID=$!
  fi
  ./scripts/deploy-byoi.sh
  [[ -n "$KC_FIX_PID" ]] && wait "$KC_FIX_PID" 2>/dev/null || true
fi
ok "BYOI ready"

# Realm-user password bookkeeping (demo hardening): a fresh Keycloak got our
# generated password — store it in a Secret.
if [[ "${SKIP_KEYCLOAK:-0}" != "1" && -n "${REALM_ADMIN_PASSWORD:-}" ]]; then
  printf '%s' "$REALM_ADMIN_PASSWORD" | "$KUBECTL" -n "$NAMESPACE" create secret generic "${CR_NAME}-realm-admin" \
    --from-file=password=/dev/stdin --dry-run=client -o yaml | "$KUBECTL" apply -f - >/dev/null
  ok "realm-user password stored in Secret ${NAMESPACE}/${CR_NAME}-realm-admin"
fi

# Post-deploy arch pass: BYOI operand images are chosen inside the prereq
# scripts, not in the CR — verify what actually got scheduled.
if [[ "$ARCH_CHECK" == "1" ]]; then
  pod_images=()
  for _pns in "$KAFKA_NAMESPACE" "$INFRA_NAMESPACE" "$KEYCLOAK_NAMESPACE"; do
    while IFS= read -r _pimg; do
      [[ -n "$_pimg" ]] && pod_images+=("$_pimg")
    done < <(byoi_pod_images "$KUBECTL" "$_pns")
  done
  if [[ ${#pod_images[@]} -gt 0 ]]; then
    assert_arch_compatible "$NODE_ARCH" ${pod_images[@]+"${pod_images[@]}"} || exit 1
    ok "BYOI pod images compatible with ${NODE_ARCH}"
  fi
fi
fi # end SKIP_PREREQS gate

# A pre-existing Keycloak keeps whatever password it was provisioned with —
# read it back for the summary (also with --skip-prereqs: the Secret lives in
# the app namespace and is ours to read).
if [[ "$PREREQS_ONLY" == "1" ]]; then
  step "Prerequisites only"
  cat <<EOF

Prerequisites are ready:
  app NS:      ${NAMESPACE}
  infra NS:    ${INFRA_NAMESPACE}
  kafka NS:    ${KAFKA_NAMESPACE}
  keycloak NS: ${KEYCLOAK_NAMESPACE}

Next steps:
  - Install the published operator:   ./scripts/demo-preprod.sh --source=registry
  - Build and install your own:        ./scripts/demo-preprod.sh --source=local
  - Console-guided install:            ./scripts/demo-preprod.sh --source=manual
  - On your own infrastructure:        ./scripts/demo-preprod.sh --skip-prereqs --cr-file <your-cmsc.yaml>
EOF
  exit 0
fi

if [[ "$SOURCE" == "manual" ]]; then
  step "Operator install (manual)"
  if [[ -n "$USER_CR_FILE" ]]; then
    echo "  warning: --cr-file with --source=manual: the instructions below point at your" >&2
    echo "  CR; the sample CR is not rendered." >&2
    CR_FILE="$USER_CR_FILE"
  else
    cp "$CR_TO_APPLY" "$CR_FILE"
    rm -f "$CR_RENDER_TMP"
    ok "CMSC rendered to ${CR_FILE}"
  fi
  print_console_access
  cat <<EOF

Install the operator through the OpenShift console:

  1. Administrator -> Cluster Settings -> Configuration -> OperatorHub -> Sources.
     Create a CatalogSource named ${CATALOG_NAME} in openshift-marketplace,
     with image ${CATALOG_IMG} and source type grpc.
  2. Wait for the catalog source to be Ready, then open Ecosystem -> Software Catalog.
     Search for Cost Management Service Operator and click Install.
  3. Verify that the catalog offers ${CATALOG_VERSION}, then choose channel ${CHANNEL},
     installation mode All namespaces on the cluster, installed namespace
     ${NAMESPACE}, and Automatic approval. Click Install.
  4. Wait until Installed Operators shows the CSV as Succeeded, then run:

        ${KUBECTL} apply -f ${CR_FILE}

Then start the demo observers:

  ./scripts/demo-preprod.sh --watch
EOF
  exit 0
fi

csv_succeeded() {
  local csv
  csv="$("$KUBECTL" -n "$NAMESPACE" get csv -o name 2>/dev/null | grep 'koku-service-operator' | head -1 || true)"
  [[ -n "$csv" ]] && [[ "$("$KUBECTL" -n "$NAMESPACE" get "$csv" -o jsonpath='{.status.phase}' 2>/dev/null || true)" == "Succeeded" ]]
}

# Best-effort exact-bundle check: only when opm is installed (never a
# mandatory dependency). Compares the operator image inside the requested
# catalog with the one the installed CSV deploys.
verify_bundle_match() {
  command -v opm >/dev/null 2>&1 || return 0
  local csv installed expected
  csv="$("$KUBECTL" -n "$NAMESPACE" get csv -o name 2>/dev/null | grep 'koku-service-operator' | head -1 || true)"
  [[ -n "$csv" ]] || return 0
  installed="$("$KUBECTL" -n "$NAMESPACE" get "$csv" -o jsonpath='{.spec.install.spec.deployments[0].spec.template.spec.containers[0].image}' 2>/dev/null || true)"
  expected="$(opm render "$CATALOG_IMG" 2>/dev/null | grep -E '^[[:space:]]*image: .*koku-service-operator@sha256:' | head -1 | sed 's/^[[:space:]]*image: //')"
  if [[ -z "$installed" || -z "$expected" ]]; then
    return 0
  fi
  if [[ "$installed" == "$expected" ]]; then
    ok "installed bundle matches ${CATALOG_IMG}"
  else
    echo "  warning: installed operator image (${installed}) differs from the bundle in" >&2
    echo "  ${CATALOG_IMG} (${expected}); a pre-existing install likely won. See --reset." >&2
  fi
}

olm_install() {
  echo "  applying CatalogSource (openshift-marketplace), OperatorGroup and Subscription (${NAMESPACE})…"
  olm_manifests "$CATALOG_IMG" "$CHANNEL" "$NAMESPACE" "$CATALOG_NAME" | "$KUBECTL" apply -f - >/dev/null

  echo "  waiting for CatalogSource ${CATALOG_NAME} to be Ready…"
  local deadline=$((SECONDS + 300))
  while (( SECONDS < deadline )); do
    if [[ "$("$KUBECTL" -n openshift-marketplace get catalogsource "$CATALOG_NAME" -o jsonpath='{.status.connectionState.lastObservedState}' 2>/dev/null || true)" == "READY" ]]; then
      break
    fi
    sleep 10
  done
  if [[ "$("$KUBECTL" -n openshift-marketplace get catalogsource "$CATALOG_NAME" -o jsonpath='{.status.connectionState.lastObservedState}' 2>/dev/null || true)" != "READY" ]]; then
    echo "error: CatalogSource ${CATALOG_NAME} did not become Ready within 5m" >&2
    "$KUBECTL" -n openshift-marketplace get catalogsource,pods >&2 || true
    exit 1
  fi
  ok "CatalogSource Ready"

  echo "  waiting for the CSV to reach Succeeded (channel ${CHANNEL}, Automatic)…"
  deadline=$((SECONDS + 600))
  while (( SECONDS < deadline )); do
    if csv_succeeded; then
      break
    fi
    sleep 10
  done
  if ! csv_succeeded; then
    echo "error: operator CSV did not reach Succeeded within 10m" >&2
    "$KUBECTL" -n "$NAMESPACE" get csv,subscription,installplan -o wide >&2 || true
    "$KUBECTL" -n openshift-marketplace get catalogsource >&2 || true
    exit 1
  fi
  local csv
  csv="$("$KUBECTL" -n "$NAMESPACE" get csv -o name 2>/dev/null | grep 'koku-service-operator' | head -1)"
  ok "CSV ${csv#*/} Succeeded"
  verify_bundle_match
}

if [[ "$SOURCE" == "registry" ]]; then
  step "Operator install (registry via OLM)"
  local_preexisting_csv=0
  if [[ -n "$(olm_existing_csvs "$KUBECTL" "$NAMESPACE")" ]]; then
    warn_existing_install "$NAMESPACE"
    local_preexisting_csv=1
  fi
  olm_install
  if [[ "$local_preexisting_csv" == "1" ]]; then
    # Cross-model review #8: do not claim an install from the new catalog when
    # the pre-existing (equal-version) CSV simply stayed in place.
    ok "kept the existing install in ${NAMESPACE} (run --reset to change version)"
  else
    ok "operator installed from ${CATALOG_IMG}"
  fi
  if [[ "$ARCH_CHECK" == "1" ]]; then
    op_img="$(installed_operator_image "$KUBECTL" "$NAMESPACE")" || op_img=""
    if [[ -n "$op_img" ]]; then
      assert_arch_compatible "$NODE_ARCH" "$op_img" || {
        echo "  The CSV is installed but its operator image cannot run on ${NODE_ARCH}." >&2
        echo "  Remove it with --reset before trying a different version." >&2
        exit 1
      }
      ok "operator image runs on ${NODE_ARCH}"
    fi
  fi
else
  step "Operator image"
BUILT_IMAGE=0
build_openshift() {
  if [[ "$(basename "$KUBECTL")" != "oc" ]]; then
    echo "error: BUILD_MODE=openshift requires oc (new-build/start-build are oc-only; KUBECTL=${KUBECTL})" >&2
    return 1
  fi
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
  local tool
  tool="$(container_tool)" || exit 1
  build_operator_image "$tool" "$NODE_ARCH" "$IMG" "$ROOT"
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
        # The detected container tool (docker with buildx, else podman);
        # openshift binary build when neither is available. The buildx
        # capability probe inside container_tool defeats the podman-docker
        # shim, which answers `docker info` but has no `docker buildx`.
        mode="$(container_tool)"
        if [[ "$mode" == "none" ]]; then
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
  IMG="$IMG" ./scripts/deploy-incluster.sh "$NAMESPACE"
fi
ok "operator running"
fi

step "Apply CostManagementServiceConfig"
if [[ -n "$USER_CR_FILE" ]]; then
  echo "  applying user-provided CR ${USER_CR_FILE}"
fi
"$KUBECTL" apply -f "$CR_TO_APPLY"
if [[ -n "${CR_RENDER_TMP:-}" ]]; then
  rm -f "$CR_RENDER_TMP"
fi
ok "CR ${NAMESPACE}/${CR_NAME} applied (issuer ${KEYCLOAK_ISSUER})"

step "Wait for UI Deployment"
echo "  watching ${NAMESPACE}/${CR_NAME} (UIReady includes the UI rollout)…"
wait_for_ui "$KUBECTL" "$NAMESPACE" "$CR_NAME" "$UI_URL" "$(should_open_browser)"
ok "UI Deployment Ready"
verify_conditions "$KUBECTL" "$NAMESPACE" "$CR_NAME" || {
  echo "error: CMSC verification failed — see the table above" >&2
  exit 1
}

step "Open the UI"
echo "  ${UI_URL}"
echo "  Keycloak realm user: admin / ${REALM_ADMIN_PASSWORD:-admin (legacy default)}"
echo "  (retrieve later: ${KUBECTL} -n ${NAMESPACE} extract secret/${CR_NAME}-realm-admin --to=-)"
if [[ "$DEMO_NO_OPEN" == "1" || "$OPEN_BROWSER" == "0" ]]; then
  skip "browser launch disabled"
elif command -v open >/dev/null 2>&1; then
  open "$UI_URL" || true
  ok "opened in default browser (cluster TLS may need a click-through)"
else
  skip "no 'open' command; visit the URL above"
fi

echo ""
demo_profile_banner
echo ""
echo "${C_BOLD}${C_GREEN}Demo complete.${C_RESET} UI: ${UI_URL}"
echo "Login: admin / ${REALM_ADMIN_PASSWORD:-admin (legacy default)}"
echo "Retrieve later: ${KUBECTL} -n ${NAMESPACE} extract secret/${CR_NAME}-realm-admin --to=-"
