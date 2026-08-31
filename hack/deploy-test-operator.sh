#!/usr/bin/env bash
# Operator local/CI deploy + pytest orchestration (chart-parity entrypoint).
#
# Fresh cluster: RHBK + AMQ Streams + S4 → in-cluster operator → CMSC → pytest.
# Does NOT use Helm (deprecated chart path). Prow (COST-7699) can invoke the same
# phases; the install step may use OLM instead of deploy-incluster.sh.
#
# Additive only — this script does NOT modify or replace:
#   - scripts/deploy-test-cost-onprem.sh (legacy chart orchestrator; still valid)
#   - hack/ci/e2e.sh (Prow/BYOI path; operator must be pre-installed)
#   - hack/demo-preprod.sh, hack/clusterbot-smoke.sh, hack/deploy-byoi.sh
#   - hack/deploy-incluster.sh or any scripts/*.sh it calls (invoked as subprocesses)
#
# No --reset / teardown: does not delete namespaces or uninstall releases.
# S4 is opt-in (--deploy-s4). Re-runs use oc apply / merge patch (idempotent-ish).
#
# Usage (from repo root):
#   # ODF / NooBaa (no S4) — default lab path when cluster has openshift-storage:
#   # Installs openshift service CA Secret + spec.objectStorage.caCertSecretName for StorageReady.
#   IMG=quay.io/<you>/koku-service-operator:<tag> \
#     ./hack/deploy-test-operator.sh --namespace cost-onprem
#
#   # Clusters without ODF (S4 stand-in):
#   IMG=... ./hack/deploy-test-operator.sh --namespace cost-onprem --deploy-s4
#
#   ./hack/deploy-test-operator.sh --tests-only --no-ui
#   ./hack/deploy-test-operator.sh --dry-run --verbose
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# shellcheck disable=SC1091
source "${ROOT}/hack/lib/deploy-test-operator.bash"

# Defaults (match cluster-bot pytest runbook).
NAMESPACE="${NAMESPACE:-cost-onprem}"
CR_NAME="${CR_NAME:-cost-onprem}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-$CR_NAME}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
S4_NAMESPACE="${S4_NAMESPACE:-s4-test}"
CMSC_SAMPLE="${CMSC_SAMPLE:-config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml}"
CMSC_READY_TIMEOUT="${CMSC_READY_TIMEOUT:-45m}"
STORAGE_CLASS="${STORAGE_CLASS:-}"
LOG_LEVEL="${LOG_LEVEL:-INFO}"

DEPLOY_S4="${DEPLOY_S4:-false}"
SKIP_RHBK=false
SKIP_KAFKA=false
SKIP_S4=false
SKIP_OPERATOR=false
SKIP_CMSC=false
SKIP_TEST=false
TESTS_ONLY=false
DRY_RUN=false
VERBOSE=false
NO_UI=false

usage() {
  sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
  cat <<'EOF'

Options:
  --namespace NAME       App / operator / CMSC namespace (default: cost-onprem)
  --cr-name NAME         CostManagementServiceConfig metadata.name (default: cost-onprem)
  --deploy-s4            Deploy S4 stand-in (clusters without ODF/NooBaa; omit on ODF labs)
  --s4-namespace NAME    S4 namespace (default: s4-test)
  --skip-rhbk            Skip Keycloak deployment
  --skip-kafka           Skip AMQ Streams / Kafka deployment
  --skip-s4              Skip S4 (use when cluster has ODF/NooBaa instead)
  --skip-operator        Skip in-cluster operator rollout (requires prior install)
  --skip-cmsc            Skip CMSC apply and wait
  --skip-test            Deploy only; do not run pytest
  --tests-only           Skip all deploy steps; run pytest only
  --no-ui                Pass --no-ui to run-pytest.sh
  --dry-run              Print planned steps without changing the cluster
  --verbose              Trace sub-script execution (bash -x)
  -h, --help             Show this help

Environment:
  IMG                    Operator image (required unless --tests-only or --skip-operator)
  KUBE_CONTEXT           Pin kubectl/oc context (optional)
  CMSC_READY_TIMEOUT     Wait for day-one CMSC conditions (default: 45m)
  ODF_S3_CA_SECRET_NAME  Secret for openshift service CA on ODF path (default: odf-s3-ca)

Coexistence (unchanged workflows):
  Chart path             scripts/deploy-test-cost-onprem.sh
  Prow / BYOI pytest     hack/ci/e2e.sh (after OLM or deploy-incluster)
  Quick smoke            hack/clusterbot-smoke.sh + deploy-incluster.sh
  UI demo                hack/demo-preprod.sh
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace)
      NAMESPACE="$2"
      shift 2
      ;;
    --cr-name)
      CR_NAME="$2"
      HELM_RELEASE_NAME="$2"
      shift 2
      ;;
    --deploy-s4)
      DEPLOY_S4=true
      shift
      ;;
    --s4-namespace)
      S4_NAMESPACE="$2"
      shift 2
      ;;
    --skip-rhbk) SKIP_RHBK=true; shift ;;
    --skip-kafka) SKIP_KAFKA=true; shift ;;
    --skip-s4) SKIP_S4=true; shift ;;
    --skip-operator) SKIP_OPERATOR=true; shift ;;
    --skip-cmsc) SKIP_CMSC=true; shift ;;
    --skip-test) SKIP_TEST=true; shift ;;
    --tests-only) TESTS_ONLY=true; shift ;;
    --no-ui) NO_UI=true; shift ;;
    --dry-run) DRY_RUN=true; shift ;;
    --verbose) VERBOSE=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      dto_log_error "unknown option: $1"
      usage >&2
      exit 2
      ;;
  esac
done

export NAMESPACE CR_NAME HELM_RELEASE_NAME KAFKA_NAMESPACE KEYCLOAK_NAMESPACE
export S4_NAMESPACE CMSC_SAMPLE CMSC_READY_TIMEOUT STORAGE_CLASS LOG_LEVEL
export DEPLOY_S4 DRY_RUN VERBOSE

if [[ "$TESTS_ONLY" == "true" ]]; then
  SKIP_RHBK=true
  SKIP_KAFKA=true
  SKIP_S4=true
  SKIP_OPERATOR=true
  SKIP_CMSC=true
fi

if [[ "$SKIP_S4" == "true" ]]; then
  DEPLOY_S4=false
fi

if [[ "$SKIP_OPERATOR" != "true" && "$TESTS_ONLY" != "true" && -z "${IMG:-}" ]]; then
  dto_log_error "IMG is required (operator container image for deploy-incluster.sh)"
  dto_log_error "  IMG=quay.io/<org>/koku-service-operator:<tag> $0 ..."
  exit 1
fi

if [[ ! -f "$CMSC_SAMPLE" ]]; then
  dto_log_error "CMSC sample not found: ${CMSC_SAMPLE}"
  exit 1
fi

dto_print_plan

if [[ "$DRY_RUN" == "true" ]]; then
  dto_log_warning "DRY RUN — no cluster changes"
  echo ""
fi

dto_check_prerequisites
dto_check_oc_connection
dto_pin_kube_context

if [[ "$TESTS_ONLY" != "true" ]]; then
  dto_create_app_namespace
  dto_deploy_rhbk
  dto_deploy_kafka
  dto_deploy_s4
  dto_deploy_operator
  dto_apply_cmsc
  dto_wait_cmsc_day_one
fi

if [[ "$SKIP_TEST" != "true" ]]; then
  dto_run_pytest
else
  dto_log_warning "Skipping pytest (--skip-test)"
fi

dto_log_success "deploy-test-operator completed"
echo ""
dto_log_info "Namespace: ${NAMESPACE}"
dto_log_info "CMSC:      ${NAMESPACE}/${CR_NAME}"
if [[ "$SKIP_TEST" == "true" ]]; then
  dto_log_info "Run tests: NAMESPACE=${NAMESPACE} HELM_RELEASE_NAME=${HELM_RELEASE_NAME} ./scripts/run-pytest.sh --no-ui -v"
fi
