#!/bin/bash
set -euo pipefail

################################################################################
# Cost Management On-Premise OpenShift with JWT Authentication Deployment Script
#
# This script orchestrates the complete setup for Cost Management on
# OpenShift by wrapping the authoritative scripts from cost-onprem-chart repository.
#
# Based on: https://github.com/insights-onprem/cost-onprem-chart/blob/main/scripts/README.md
# Section: JWT Authentication Setup
#
# Usage:
#   ./deploy-test-cost-onprem.sh [OPTIONS]
#
# Options:
#
#   Execution mode:
#   --skip-deploy             Skip all deployment steps, run tests only
#   --skip-chart-tests        Skip chart pytest suite
#   --iqe-only                Run only IQE tests (skip deployment and chart tests)
#   --run-iqe                 Run IQE cost-management tests after deployment
#   --dry-run                 Show what would be executed without running
#
#   Deployment control:
#   --skip-rhbk               Skip Red Hat Build of Keycloak (RHBK) deployment
#   --skip-kafka              Skip Kafka/AMQ Streams deployment
#   --skip-helm               Skip COST Helm chart installation
#   --skip-tls                Skip TLS certificate setup
#   --skip-image-override     Skip creating custom values file for image override
#   --deploy-s4               Deploy S4 (Super Simple Storage Service) for S3-compatible storage
#   --s4-namespace NAME       S4 deployment namespace (default: s4-test)
#   --namespace NAME          Target namespace (default: cost-onprem)
#   --image-tag TAG           Custom image tag for cost-onprem-ocp-backend services
#   --use-local-chart         Use local Helm chart instead of GitHub release
#   --devel                   Include pre-release (rc) charts in Helm installation
#   --chart-version VERSION   Pin a specific Helm chart version (e.g., 0.2.9, 0.3.0-rc1)
#   --sizing-profile PROFILE  Apply a sizing profile on deploy: small, medium, large, xlarge.
#                             Passes cost-onprem/values-<PROFILE>.yaml as an additional -f overlay.
#                             Small = chart defaults (no-op); medium+ adds resource/replica overrides.
#
#   Test options:
#   --iqe-marker EXPR         Pytest marker for IQE tests (default: cost_ocp_on_prem)
#   --iqe-profile PROFILE     IQE test profile: smoke, extended, stable, full (default: stable)
#   --listener-cpu LIMIT      Temporarily set listener CPU limit (e.g., 500m, 1000m, 'max', or 'none' to skip).
#                             Recommended: use 'max' for --run-perf / --perf-only runs.
#   --include-ui              Include UI tests (requires Playwright system dependencies)
#   --run-perf                Run performance tests after deployment (FLPATH-4036)
#   --perf-profile PROFILE    Performance profile: baseline, small, medium, large (default: baseline)
#   --perf-suite SUITES       Performance suite(s): all, api, ros, ingestion, scale, soak, valkey, db, kafka, celery, stress, stress_ramp, stress_recovery
#                             Comma-separated for multiple (e.g., ros,ingestion). Default: all
#   --perf-only               Run only performance tests (skip deployment and chart tests)
#   --skip-profile-config     Skip apply_perf_profile_config() — test with chart defaults only
#
#   Observability options (FLPATH-4061):
#   --deploy-observability    Deploy postgres_exporter, valkey-exporter, and celery-exporter for metrics
#   --collect-metrics         Collect Prometheus metrics during performance tests
#   --upload-metrics          Upload metrics to S3 after tests (requires S3_BUCKET to be set)
#   --metrics-interval SECS   Metrics collection interval in seconds (default: 30)
#   --grafana-links           Enable Grafana snapshot/dashboard links (disabled by default)
#   --skip-grafana-links      Explicitly skip Grafana links (default behavior)
#
#   Other:
#   --save-versions [FILE]    Save deployment version info to JSON file (default: version_info.json)
#   --verbose                 Enable verbose output
#   --help                    Display this help message
#
#   Backward-compatible aliases:
#   --tests-only              Alias for --skip-deploy
#   --skip-test               Alias for --skip-chart-tests
#
# Environment Variables:
#   KUBECONFIG               Path to kubeconfig file (default: ~/.kube/config)
#   KUBEADMIN_PASSWORD_FILE  Path to kubeadmin password file
#   SHARED_DIR               Shared directory containing kubeadmin-password
#   OPENSHIFT_API            OpenShift API URL (auto-detected from kubeconfig)
#   OPENSHIFT_USERNAME       OpenShift username (default: kubeadmin)
#   OPENSHIFT_PASSWORD       OpenShift password (auto-detected from files)
#   OPENSHIFT_VALUES_FILE    Helm values file path relative to repo root (default: openshift-values.yaml)
#
# Note: This script will automatically login to OpenShift using credentials from:
#       1. KUBECONFIG file (for API URL)
#       2. KUBEADMIN_PASSWORD_FILE or SHARED_DIR/kubeadmin-password (for password)
#       If already logged in, it will skip the login step.
#
# Prerequisites:
#   - oc CLI installed and configured
#   - kubectl CLI installed and configured
#   - helm CLI installed (v3+)
#   - yq installed for YAML/JSON processing
#   - OpenShift cluster with admin access
#
# Examples:
#   # Full deployment + chart tests (default)
#   ./deploy-test-cost-onprem.sh --namespace cost-onprem --verbose
#
#   # Deploy only — skip all tests
#   ./deploy-test-cost-onprem.sh --skip-chart-tests
#
#   # Run chart tests against existing deployment
#   ./deploy-test-cost-onprem.sh --skip-deploy
#
#   # Run only IQE tests with listener CPU boost
#   ./deploy-test-cost-onprem.sh --iqe-only --listener-cpu max --iqe-profile smoke
#
#   # Full deployment + chart tests + IQE tests
#   ./deploy-test-cost-onprem.sh --run-iqe --iqe-profile smoke
#
#   # Skip RHBK if already deployed
#   ./deploy-test-cost-onprem.sh --skip-rhbk
#
#   # Dry run to preview what would execute
#   ./deploy-test-cost-onprem.sh --dry-run --verbose
#
# Validation:
#   Flag parsing is tested by .github/workflows/validate-deploy-test-script.yml
#   which runs --dry-run for every flag permutation. Run locally with:
#     ./scripts/qe/test-gh-workflow-locally.sh .github/workflows/validate-deploy-test-script.yml
#
################################################################################

# Script metadata
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Default configuration
NAMESPACE="${NAMESPACE:-cost-onprem}"
USE_LOCAL_CHART="${USE_LOCAL_CHART:-false}"
USE_HELM_DEVEL="${USE_HELM_DEVEL:-false}"
CHART_VERSION="${CHART_VERSION:-}"
VERBOSE="${VERBOSE:-false}"
DRY_RUN="${DRY_RUN:-false}"
TESTS_ONLY="${TESTS_ONLY:-false}"
INCLUDE_UI="${INCLUDE_UI:-false}"
RUN_IQE="${RUN_IQE:-false}"
IQE_MARKER="${IQE_MARKER:-cost_ocp_on_prem}"
IQE_PROFILE="${IQE_PROFILE:-stable}"
RUN_PERF="${RUN_PERF:-false}"
PERF_PROFILE="${PERF_PROFILE:-baseline}"
PERF_SUITE="${PERF_SUITE:-all}"
PERF_ONLY="${PERF_ONLY:-false}"
DEPLOY_OBSERVABILITY="${DEPLOY_OBSERVABILITY:-false}"
COLLECT_METRICS="${COLLECT_METRICS:-false}"
UPLOAD_METRICS="${UPLOAD_METRICS:-false}"
METRICS_INTERVAL="${METRICS_INTERVAL:-30}"

# Performance output directory - unified structure for metrics + test results + reports
# Format: {PERF_OUTPUT_DIR}/{TEST_RUN_ID}/ with subdirs: metrics/, results/, reports/
# Default: PROJECT_ROOT/tests/perf-runs (absolute path to avoid issues when pytest runs from tests/)
PERF_OUTPUT_DIR="${PERF_OUTPUT_DIR:-${PROJECT_ROOT}/tests/perf-runs}"
# TEST_RUN_ID is auto-generated: {chart_version}-{perf_profile}-{epoch}
TEST_RUN_ID="${TEST_RUN_ID:-}"
SAVE_VERSIONS="${SAVE_VERSIONS:-false}"
VERSION_INFO_FILE="${VERSION_INFO_FILE:-version_info.json}"
LISTENER_CPU_LIMIT="${LISTENER_CPU_LIMIT:-}"
SKIP_PROFILE_CONFIG="${SKIP_PROFILE_CONFIG:-false}"

# S4 deployment configuration
DEPLOY_S4="${DEPLOY_S4:-false}"
S4_NAMESPACE="${S4_NAMESPACE:-s4-test}"

# CPU boost state tracking (for cleanup on exit)
CPU_BOOST_APPLIED=false

# OpenShift authentication
KUBECONFIG="${KUBECONFIG:-${HOME}/.kube/config}"
OPENSHIFT_USERNAME="${OPENSHIFT_USERNAME:-kubeadmin}"
OPENSHIFT_API="${OPENSHIFT_API:-}"
OPENSHIFT_PASSWORD="${OPENSHIFT_PASSWORD:-}"
KUBEADMIN_PASSWORD_FILE="${KUBEADMIN_PASSWORD_FILE:-}"
SHARED_DIR="${SHARED_DIR:-}"

# Local scripts directory (this script sits alongside the other scripts)
LOCAL_SCRIPTS_DIR="${SCRIPT_DIR}"
SCRIPT_DEPLOY_RHBK="deploy-rhbk.sh"  # Red Hat Build of Keycloak (RHBK)
SCRIPT_DEPLOY_KAFKA="deploy-kafka.sh"
SCRIPT_DEPLOY_S4="deploy-s4-test.sh"  # S4 (Super Simple Storage Service)
SCRIPT_INSTALL_HELM="install-cmsc.sh"
SCRIPT_SETUP_TLS="setup-cost-mgmt-tls.sh"
OPENSHIFT_VALUES_FILE="${OPENSHIFT_VALUES_FILE:-openshift-values.yaml}"
SIZING_PROFILE="${SIZING_PROFILE:-}"

# Step flags (default: run all steps)
SKIP_RHBK=false  # Red Hat Build of Keycloak
SKIP_KAFKA=false
SKIP_HELM=false
SKIP_TLS=false
SKIP_TEST=false
SKIP_GRAFANA_LINKS=${SKIP_GRAFANA_LINKS:-true}  # Skip Grafana snapshot/links (default: skip, local Grafana rarely accessible)

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

################################################################################
# Cleanup trap for CPU boost
################################################################################

cleanup_on_exit() {
    local exit_code=$?
    
    # Stop metrics collection if running
    if [[ -n "${METRICS_COLLECTOR_PID:-}" ]]; then
        echo ""
        echo -e "${YELLOW}[CLEANUP] Stopping metrics collection...${NC}"
        kill -TERM "${METRICS_COLLECTOR_PID}" 2>/dev/null || true
    fi
    
    # Reset CPU boost if applied
    if [[ "${CPU_BOOST_APPLIED}" == "true" ]] && [[ -n "${ORIGINAL_LISTENER_CPU_LIMIT}" ]]; then
        echo ""
        echo -e "${YELLOW}[CLEANUP] Resetting listener CPU to original values...${NC}"
        reset_listener_cpu 2>/dev/null || true
    fi
    exit $exit_code
}

# Register cleanup trap for any exit (including errors from set -e)
trap cleanup_on_exit EXIT

################################################################################
# Logging functions
################################################################################

log_info() {
    echo -e "${BLUE}ℹ INFO:${NC} $*"
}

log_success() {
    echo -e "${GREEN}✅ SUCCESS:${NC} $*"
}

log_warning() {
    echo -e "${YELLOW}⚠ WARNING:${NC} $*"
}

log_error() {
    echo -e "${RED}❌ ERROR:${NC} $*" >&2
}

log_step() {
    echo -e "${CYAN}▶${NC} $*"
}

log_verbose() {
    if [[ "${VERBOSE}" == "true" ]]; then
        echo -e "${CYAN}[VERBOSE]${NC} $*" >&2
    fi
}

################################################################################
# Sourced Libraries
################################################################################

[[ -f "${SCRIPT_DIR}/lib/listener-cpu.sh" ]] && source "${SCRIPT_DIR}/lib/listener-cpu.sh"
[[ -f "${SCRIPT_DIR}/lib/perf-observability.sh" ]] && source "${SCRIPT_DIR}/lib/perf-observability.sh"
[[ -f "${SCRIPT_DIR}/lib/perf-testing.sh" ]] && source "${SCRIPT_DIR}/lib/perf-testing.sh"

################################################################################
# Utility functions
################################################################################

show_help() {
    sed -n '/^# Usage:/,/^################################################################################$/p' "$0" | sed 's/^# \?//'
    exit 0
}

check_prerequisites() {
    log_step "Checking prerequisites"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Skipping prerequisite checks"
        return 0
    fi

    local missing_tools=()

    # Check required tools
    for tool in oc kubectl helm yq; do
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=("$tool")
        else
            log_verbose "Found: $tool ($(command -v "$tool"))"
        fi
    done

    if [[ ${#missing_tools[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_error "Please install missing tools and try again"
        log_error ""
        log_error "Installation instructions:"
        log_error "  macOS:  brew install kubectl yq"
        log_error "  Linux:  kubectl: https://kubernetes.io/docs/tasks/tools/install-kubectl-linux/  |  yq: https://github.com/mikefarah/yq#install"
        exit 1
    fi

    log_success "All required tools are installed"
}

detect_openshift_credentials() {
    log_info "Detecting OpenShift credentials from environment..."

    # Detect API URL from kubeconfig if not set
    if [[ -z "${OPENSHIFT_API}" ]] && [[ -f "${KUBECONFIG}" ]]; then
        OPENSHIFT_API=$(yq e '.clusters[0].cluster.server' "${KUBECONFIG}" 2>/dev/null || echo "")
        if [[ -n "${OPENSHIFT_API}" ]]; then
            log_verbose "Detected API URL from kubeconfig: ${OPENSHIFT_API}"
        fi
    fi

    # Detect password from files if not set
    if [[ -z "${OPENSHIFT_PASSWORD}" ]]; then
        if [[ -n "${KUBEADMIN_PASSWORD_FILE}" ]] && [[ -s "${KUBEADMIN_PASSWORD_FILE}" ]]; then
            OPENSHIFT_PASSWORD="$(cat "${KUBEADMIN_PASSWORD_FILE}")"
            log_verbose "Loaded password from KUBEADMIN_PASSWORD_FILE"
        elif [[ -n "${SHARED_DIR}" ]] && [[ -s "${SHARED_DIR}/kubeadmin-password" ]]; then
            OPENSHIFT_PASSWORD="$(cat "${SHARED_DIR}/kubeadmin-password")"
            log_verbose "Loaded password from SHARED_DIR/kubeadmin-password"
        fi
    fi
}

login_to_openshift() {
    log_step "Logging into OpenShift"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would login to OpenShift"
        return 0
    fi

    # Detect credentials from environment
    detect_openshift_credentials

    # Check if credentials are available
    if [[ -z "${OPENSHIFT_API}" ]]; then
        log_error "OPENSHIFT_API not set and could not be detected from kubeconfig"
        log_error "Please set OPENSHIFT_API environment variable or ensure KUBECONFIG is valid"
        return 1
    fi

    if [[ -z "${OPENSHIFT_PASSWORD}" ]]; then
        log_error "OPENSHIFT_PASSWORD not set and could not be detected from files"
        log_error "Please set one of:"
        log_error "  - OPENSHIFT_PASSWORD environment variable"
        log_error "  - KUBEADMIN_PASSWORD_FILE pointing to password file"
        log_error "  - SHARED_DIR containing kubeadmin-password file"
        return 1
    fi

    # Configure kubeconfig to skip TLS verification
    if [[ -f "${KUBECONFIG}" ]]; then
        log_verbose "Configuring kubeconfig to skip TLS verification..."
        yq -i 'del(.clusters[].cluster.certificate-authority-data) | .clusters[].cluster.insecure-skip-tls-verify=true' "${KUBECONFIG}" 2>/dev/null || true
    fi

    # Attempt login
    log_info "Logging in as ${OPENSHIFT_USERNAME} to ${OPENSHIFT_API}..."
    if oc login "${OPENSHIFT_API}" \
        --username="${OPENSHIFT_USERNAME}" \
        --password="${OPENSHIFT_PASSWORD}" \
        --insecure-skip-tls-verify=true &> /dev/null; then
        log_success "Successfully logged into OpenShift"
    else
        log_error "Failed to login to OpenShift"
        log_error "Please verify credentials and API URL"
        return 1
    fi
}

check_oc_connection() {
    log_step "Verifying OpenShift connection"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would verify OpenShift connection"
        return 0
    fi

    # Check if already logged in
    if ! oc whoami &> /dev/null; then
        log_info "Not currently logged into OpenShift, attempting automatic login..."

        # Try to login automatically
        if ! login_to_openshift; then
            log_error "Automatic login failed"
            log_error ""
            log_error "Manual login options:"
            log_error "  1. Set environment variables:"
            log_error "     export OPENSHIFT_API='https://api.example.com:6443'"
            log_error "     export OPENSHIFT_PASSWORD='your-password'"
            log_error ""
            log_error "  2. Or login manually:"
            log_error "     oc login https://api.example.com:6443"
            log_error ""
            exit 1
        fi
    else
        log_success "Already logged into OpenShift"
    fi

    local current_user
    current_user=$(oc whoami)
    local current_server
    current_server=$(oc whoami --show-server)

    log_success "Connected to OpenShift as: ${current_user}"
    log_info "Server: ${current_server}"

    # Check if user has admin privileges
    if oc auth can-i create clusterrole &> /dev/null; then
        log_success "User has cluster-admin privileges"
    else
        log_warning "User may not have sufficient privileges for cluster-scoped resources"
        log_warning "Some deployment steps may fail without admin access"
    fi
}

execute_script() {
    local script_name="$1"
    shift
    local script_path="${LOCAL_SCRIPTS_DIR}/${script_name}"

    if [[ ! -f "${script_path}" ]]; then
        log_error "Script not found: ${script_path}"
        return 1
    fi
    if [[ ! -x "${script_path}" ]]; then
        chmod +x "${script_path}" 2>/dev/null || true
    fi

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would execute: ${script_path} $*"
        return 0
    fi

    log_info "Executing: ${script_path} $*"

    local exit_code=0
    if [[ "${VERBOSE}" == "true" ]]; then
        bash -x "${script_path}" "$@" || exit_code=$?
    else
        "${script_path}" "$@" || exit_code=$?
    fi

    return ${exit_code}
}

create_namespace() {
    log_step "Creating namespace: ${NAMESPACE}"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would create namespace ${NAMESPACE}"
        return 0
    fi

    if oc get namespace "${NAMESPACE}" &> /dev/null; then
        log_info "Namespace ${NAMESPACE} already exists"
    else
        oc create namespace "${NAMESPACE}"
        log_success "Created namespace: ${NAMESPACE}"
    fi

    # Label namespace for Cost Management Metrics Operator
    log_info "Labeling namespace for Cost Management Metrics Operator..."
    oc label namespace "${NAMESPACE}" cost_management_optimizations=true --overwrite
    log_success "Namespace labeled successfully"
}

################################################################################
# Deployment steps
################################################################################

deploy_rhbk() {
    if [[ "${SKIP_RHBK}" == "true" ]]; then
        log_warning "Skipping Red Hat Build of Keycloak (RHBK) deployment (--skip-rhbk)"
        return 0
    fi

    log_step "Deploying Red Hat Build of Keycloak (RHBK) (1/6)"

    # Export environment variables for RHBK script
    # export NAMESPACE="${NAMESPACE}"

    if [[ "${VERBOSE}" == "true" ]]; then
        export VERBOSE="true"
    fi

    local rhbk_args=()
    local chart_values="${PROJECT_ROOT}/cost-onprem/values.yaml"
    if [[ -f "${chart_values}" ]]; then
        rhbk_args+=(-f "${chart_values}")
        log_info "Provisioning Keycloak users from: ${chart_values}"
    fi

    if ! execute_script "${SCRIPT_DEPLOY_RHBK}" "${rhbk_args[@]}"; then
        log_error "Red Hat Build of Keycloak (RHBK) deployment failed"
        exit 1
    fi

    log_success "Red Hat Build of Keycloak (RHBK) deployment completed"
}

deploy_kafka() {
    if [[ "${SKIP_KAFKA}" == "true" ]]; then
        log_warning "Skipping Kafka/AMQ Streams deployment (--skip-kafka)"
        return 0
    fi

    log_step "Deploying Kafka/AMQ Streams (2/6)"

    # Export environment variables for AMQ Streams script
    # export KAFKA_NAMESPACE="${NAMESPACE}"
    export STORAGE_CLASS="${STORAGE_CLASS:-}"

    log_verbose "Using storage class: ${STORAGE_CLASS}"

    if [[ "${VERBOSE}" == "true" ]]; then
        export VERBOSE="true"
    fi

    if ! execute_script "${SCRIPT_DEPLOY_KAFKA}"; then
        log_error "Kafka/AMQ Streams deployment failed"
        exit 1
    fi

    log_success "Kafka/AMQ Streams deployment completed"
}

# Copy S4/RGW credentials into the target namespace as cost-onprem-storage-credentials.
# deploy-s4-test.sh creates s4-credentials; the operator/CMSC expect cost-onprem-storage-credentials.
copy_s4_storage_credentials() {
    local source_ns="$1"
    local target_ns="$2"
    local storage_secret="cost-onprem-storage-credentials"
    local source_secret=""
    local candidate

    for candidate in s4-credentials "${storage_secret}"; do
        if kubectl get secret "${candidate}" -n "${source_ns}" >/dev/null 2>&1; then
            source_secret="${candidate}"
            break
        fi
    done

    if [[ -z "${source_secret}" ]]; then
        log_error "No S4 storage secret (s4-credentials or ${storage_secret}) in ${source_ns}"
        return 1
    fi

    local access_key
    local secret_key
    access_key=$(kubectl get secret "${source_secret}" -n "${source_ns}" -o jsonpath='{.data.access-key}' | base64 -d)
    secret_key=$(kubectl get secret "${source_secret}" -n "${source_ns}" -o jsonpath='{.data.secret-key}' | base64 -d)

    if [[ -z "${access_key}" || -z "${secret_key}" ]]; then
        log_error "Secret ${source_ns}/${source_secret} is missing access-key or secret-key"
        return 1
    fi

    if ! kubectl create secret generic "${storage_secret}" \
        --namespace="${target_ns}" \
        --from-literal=access-key="${access_key}" \
        --from-literal=secret-key="${secret_key}" \
        --dry-run=client -o yaml | kubectl apply -f -; then
        log_error "Failed to apply ${storage_secret} in ${target_ns}"
        return 1
    fi
    log_success "Storage credentials copied from ${source_ns}/${source_secret} to ${target_ns}/${storage_secret}"
}

deploy_s4() {
    if [[ "${DEPLOY_S4}" != "true" ]]; then
        log_verbose "Skipping S4 deployment (--deploy-s4 not specified)"
        return 0
    fi

    log_step "Deploying S4 (Super Simple Storage Service) (3/6)"
    log_info "S4 namespace: ${S4_NAMESPACE}"

    # Export environment variables for S4 script
    export S4_RELEASE_TAG="${S4_RELEASE_TAG:-}"
    export S4_REPO="${S4_REPO:-}"
    export STORAGE_SIZE="${STORAGE_SIZE:-}"
    export STORAGE_CLASS="${STORAGE_CLASS:-}"

    if [[ "${VERBOSE}" == "true" ]]; then
        export VERBOSE="true"
    fi

    # Pass S4_NAMESPACE as first argument to deploy-s4-test.sh
    if ! execute_script "${SCRIPT_DEPLOY_S4}" "${S4_NAMESPACE}"; then
        log_error "S4 deployment failed"
        log_info "To troubleshoot S4 deployment:"
        log_info "  1. Check S4 pod status: kubectl get pods -n ${S4_NAMESPACE}"
        log_info "  2. Check S4 pod logs: kubectl logs -n ${S4_NAMESPACE} -l app.kubernetes.io/name=s4"
        log_info "  3. Check S4 service: kubectl get svc s4 -n ${S4_NAMESPACE}"
        log_info "  4. Clean up S4: ${LOCAL_SCRIPTS_DIR}/${SCRIPT_DEPLOY_S4} ${S4_NAMESPACE} cleanup"
        exit 1
    fi

    # Set environment variables for Helm installation to use S4
    export S3_ENDPOINT="s4.${S4_NAMESPACE}.svc.cluster.local"
    export S3_PORT="7480"
    export S3_USE_SSL="false"

    # deploy-s4-test.sh creates s4-credentials; operator/CMSC expect cost-onprem-storage-credentials.
    log_info "Syncing storage credentials from ${S4_NAMESPACE} to ${NAMESPACE}..."
    if ! copy_s4_storage_credentials "${S4_NAMESPACE}" "${NAMESPACE}"; then
        log_error "S4 credential sync failed — cannot continue with stale or missing storage credentials"
        log_info "Check: kubectl get secret s4-credentials -n ${S4_NAMESPACE}"
        log_info "Manual sync steps: docs/development/clusterbot-operator-pytest.md (S4 credential gap)"
        exit 1
    fi

    log_success "S4 deployment completed"
    log_info "S3 endpoint configured: ${S3_ENDPOINT}:${S3_PORT} (SSL: ${S3_USE_SSL})"
    log_info "Storage credentials secret: cost-onprem-storage-credentials (in ${NAMESPACE})"
}

apply_sizing_profile() {
    local release="${HELM_RELEASE_NAME:-cost-onprem}"
    local namespace="${NAMESPACE:-cost-onprem}"
    local sizing_file="${PROJECT_ROOT}/cost-onprem/values-${SIZING_PROFILE}.yaml"

    if [[ ! -f "${sizing_file}" ]]; then
        log_error "Sizing profile '${SIZING_PROFILE}' not found at: ${sizing_file}"
        log_error "Valid profiles: small, medium, large, xlarge"
        exit 1
    fi

    log_step "Applying sizing profile '${SIZING_PROFILE}' to existing deployment"

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would run helm upgrade ${release} --reuse-values -f ${sizing_file}"
        return 0
    fi

    local chart_ref
    if [[ "${USE_LOCAL_CHART:-false}" == "true" ]]; then
        chart_ref="${PROJECT_ROOT}/cost-onprem"
    else
        chart_ref="${release}"
    fi

    if ! helm upgrade "${release}" "${chart_ref}" \
            --reuse-values \
            --namespace "${namespace}" \
            -f "${sizing_file}" \
            --wait --timeout 5m 2>&1; then
        log_error "Failed to apply sizing profile '${SIZING_PROFILE}'"
        return 1
    fi

    log_success "Sizing profile '${SIZING_PROFILE}' applied"

    # Restart gateway so Envoy picks up timeout changes (PERF-FINDING-020)
    local gw_deploy="${release}-gateway"
    if oc rollout restart deployment "${gw_deploy}" -n "${namespace}" 2>/dev/null; then
        if oc rollout status deployment "${gw_deploy}" -n "${namespace}" --timeout=2m 2>/dev/null; then
            log_success "Gateway restarted (Envoy config reloaded)"
        else
            log_warning "Gateway rollout did not stabilize — Envoy may still use old timeouts"
        fi
    fi
}

deploy_helm_chart() {
    if [[ "${SKIP_HELM}" == "true" ]]; then
        # When deploy is skipped but --sizing-profile is set, apply the overlay
        # to the existing release so the cluster matches the requested profile.
        if [[ -n "${SIZING_PROFILE}" ]]; then
            apply_sizing_profile
        else
            log_warning "Skipping Cost On-Prem Helm chart installation (--skip-helm)"
        fi
        return 0
    fi

    log_step "Deploying Cost On-Prem Helm chart (4/6)"

    # Use the values file (default: openshift-values.yaml, configurable via OPENSHIFT_VALUES_FILE)
    local values_file="${PROJECT_ROOT}/${OPENSHIFT_VALUES_FILE}"
    download_openshift_values "${values_file}"
    export VALUES_FILE="${values_file}"

    # Sizing profile overlay (--sizing-profile small|medium|large|xlarge)
    if [[ -n "${SIZING_PROFILE}" ]]; then
        local sizing_file="${PROJECT_ROOT}/cost-onprem/values-${SIZING_PROFILE}.yaml"
        if [[ ! -f "${sizing_file}" ]]; then
            log_error "Sizing profile '${SIZING_PROFILE}' not found at: ${sizing_file}"
            log_error "Valid profiles: small, medium, large, xlarge"
            exit 1
        fi
        export SIZING_VALUES_FILE="${sizing_file}"
        log_info "Sizing profile: ${SIZING_PROFILE} (${sizing_file})"
    fi

    # Export environment variables for Helm script
    export NAMESPACE="${NAMESPACE}"
    export JWT_AUTH_ENABLED="true"
    export USE_LOCAL_CHART="${USE_LOCAL_CHART}"
    export USE_HELM_DEVEL="${USE_HELM_DEVEL}"
    [[ -n "${CHART_VERSION}" ]] && export CHART_VERSION="${CHART_VERSION}"
    # Note: S3 setup behavior depends on values.yaml configuration:
    # - If objectStorage.endpoint is set: Script skips S3 auto-detection and bucket creation
    # - If not set: Script auto-detects (S4, NooBaa, external OBC) and creates buckets
    # SKIP_S3_SETUP=true can be used to skip bucket creation in CI environments
    # Pytests have their own S3 preflight checks that will setup the S3 buckets if they are not already present.

    if [[ "${VERBOSE}" == "true" ]]; then
        export VERBOSE="true"
    fi

    if ! execute_script "${SCRIPT_INSTALL_HELM}"; then
        log_error "Helm chart deployment failed"
        log_info ""
        log_info "To troubleshoot:"
        log_info "  1. Check Helm release status: helm list -n ${NAMESPACE}"
        log_info "  2. Check pod status: oc get pods -n ${NAMESPACE}"
        log_info "  3. View pod logs: oc logs -n ${NAMESPACE} <pod-name>"
        log_info "  4. Check events: oc get events -n ${NAMESPACE} --sort-by='.lastTimestamp'"
        exit 1
    fi

    log_success "Cost On-Prem Helm chart deployment completed"
}

download_openshift_values() {
    local values_file="$1"

    log_info "Using local OpenShift values file from repository"
    log_verbose "Path: ${values_file}"
    if [[ ! -f "${values_file}" ]]; then
        log_warning "OpenShift values file not found at: ${values_file} (skipping; CMSC sample apply does not require it)"
        return 0
    fi

    if [[ "${VERBOSE}" == "true" ]]; then
        log_verbose "Values file contents (first 30 lines):"
        head -30 "${values_file}" | while IFS= read -r line; do
            log_verbose "  ${line}"
        done
    fi
}

setup_tls() {
    if [[ "${SKIP_TLS}" == "true" ]]; then
        log_warning "Skipping TLS certificate setup (--skip-tls)"
        return 0
    fi

    log_step "Configuring TLS certificates (5/6)"

    # Export environment variables for TLS script
    export NAMESPACE="${NAMESPACE}"

    if [[ "${VERBOSE}" == "true" ]]; then
        export VERBOSE="true"
    fi

    execute_script "${SCRIPT_SETUP_TLS}"

    log_success "TLS certificate setup completed"
}


run_tests() {
    # Apply listener CPU boost early - benefits both chart tests and IQE tests
    # Note: CPU_BOOST_APPLIED is a global variable, cleanup is handled by trap
    if [[ -n "${LISTENER_CPU_LIMIT}" ]] && [[ "${LISTENER_CPU_LIMIT}" != "none" ]]; then
        if validate_cpu_limit "${LISTENER_CPU_LIMIT}"; then
            local effective_cpu_limit="${LISTENER_CPU_LIMIT}"
            if [[ "${LISTENER_CPU_LIMIT}" == "max" ]]; then
                calculate_max_listener_cpu
                effective_cpu_limit="${MAX_LISTENER_CPU}m"
                log_info "Calculated maximum available CPU: ${effective_cpu_limit}"
            fi
            
            log_step "Setting listener CPU limit to ${effective_cpu_limit}"
            if set_listener_cpu "${effective_cpu_limit}"; then
                CPU_BOOST_APPLIED=true
            else
                log_warning "Continuing with current listener CPU"
            fi
        fi
    fi
    
    # Helper to reset CPU (also called by trap on exit)
    cleanup_cpu_boost() {
        if [[ "${CPU_BOOST_APPLIED}" == "true" ]]; then
            reset_listener_cpu
            CPU_BOOST_APPLIED=false
        fi
    }
    
    if [[ "${SKIP_TEST}" == "true" ]]; then
        log_warning "Skipping cost-onprem chart tests (--skip-chart-tests)"
        # Still run IQE tests if requested
        if [[ "${RUN_IQE}" == "true" ]]; then
            run_iqe_tests
        fi
        # Still run performance tests if requested
        if [[ "${RUN_PERF}" == "true" ]]; then
            if type run_performance_tests &>/dev/null; then
                run_performance_tests
            elif [[ "${DRY_RUN}" == "true" ]]; then
                log_warning "Performance testing library not available (scripts/lib/perf-testing.sh)"
            else
                log_error "Performance testing library not found (scripts/lib/perf-testing.sh)"
                return 1
            fi
        fi
        cleanup_cpu_boost
        return 0
    fi

    log_step "Running cost-onprem chart tests (6/6)"

    # Ensure we're logged in to OpenShift for JWT test
    if [[ "${DRY_RUN}" != "true" ]]; then
        if ! oc whoami -t &> /dev/null; then
            log_info "Not logged in to OpenShift with a user that has an available token, attempting login for JWT test..."
            if ! login_to_openshift; then
                log_warning "Failed to login to OpenShift, skipping JWT test"
                # Still run IQE tests if requested
                if [[ "${RUN_IQE}" == "true" ]]; then
                    run_iqe_tests
                fi
                return 0
            fi
        fi
    fi
    
    # Export environment variables for pytest
    export NAMESPACE="${NAMESPACE}"
    export HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
    export KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"

    if [[ "${VERBOSE}" == "true" ]]; then
        export VERBOSE="true"
    fi
    
    # Run pytest test suite
    local pytest_script="${LOCAL_SCRIPTS_DIR}/run-pytest.sh"
    if [[ ! -x "${pytest_script}" ]]; then
        log_error "Pytest runner not found at: ${pytest_script}"
        exit 1
    fi
    
    log_info "Running pytest test suite..."
    
    # Build pytest arguments
    local pytest_args=()
    if [[ "${VERBOSE}" == "true" ]]; then
        pytest_args+=("-v")
    fi
    if [[ "${INCLUDE_UI}" == "true" ]]; then
        # Override default "not ui" marker to include UI tests
        pytest_args+=("-m" "")
        log_info "Including UI tests (Playwright)"
    fi
    
    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would execute: ${pytest_script} ${pytest_args[*]:-}"
        return 0
    fi
    
    # Track test failures - continue running all tests, fail at the end
    local chart_tests_failed=false
    local iqe_tests_failed=false
    local perf_tests_failed=false
    
    # Run chart tests
    # Note: cost_validation tests have their own E2E setup with 300s provider timeout
    if ! "${pytest_script}" ${pytest_args[@]+"${pytest_args[@]}"}; then
        log_error "Pytest test suite failed"
        log_info "JUnit report available at: tests/reports/junit.xml"
        chart_tests_failed=true
    else
        log_success "Pytest test suite completed"
    fi
    
    # Run IQE tests if requested (continue even if chart tests failed)
    if [[ "${RUN_IQE}" == "true" ]]; then
        if ! run_iqe_tests; then
            iqe_tests_failed=true
        fi
    fi
    
    # Run performance tests if requested (continue even if other tests failed)
    if [[ "${RUN_PERF}" == "true" ]]; then
        if type run_performance_tests &>/dev/null; then
            if ! run_performance_tests; then
                perf_tests_failed=true
            fi
        elif [[ "${DRY_RUN}" == "true" ]]; then
            log_warning "Performance testing library not available (scripts/lib/perf-testing.sh)"
        else
            log_error "Performance testing library not found (scripts/lib/perf-testing.sh)"
            perf_tests_failed=true
        fi
    fi
    
    # Reset listener CPU after all tests
    cleanup_cpu_boost
    
    # Report overall status
    if [[ "${chart_tests_failed}" == "true" ]] || [[ "${iqe_tests_failed}" == "true" ]] || [[ "${perf_tests_failed}" == "true" ]]; then
        echo ""
        log_error "Test failures detected:"
        [[ "${chart_tests_failed}" == "true" ]] && log_error "  - Chart tests (pytest) failed"
        [[ "${iqe_tests_failed}" == "true" ]] && log_error "  - IQE tests failed"
        [[ "${perf_tests_failed}" == "true" ]] && log_error "  - Performance tests failed"
        return 1
    fi
}

################################################################################
# IQE Test Execution
################################################################################

run_iqe_tests() {
    log_step "Running IQE cost-management tests"
    
    local iqe_script="${LOCAL_SCRIPTS_DIR}/run-iqe-tests.sh"
    if [[ ! -x "${iqe_script}" ]]; then
        log_error "IQE test runner not found at: ${iqe_script}"
        return 1
    fi
    
    log_info "Running IQE tests with profile: ${IQE_PROFILE}, marker: ${IQE_MARKER}"
    
    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would execute: ${iqe_script} --namespace ${NAMESPACE} --profile ${IQE_PROFILE} --marker '${IQE_MARKER}'"
        return 0
    fi
    
    # Export environment variables for IQE script
    export NAMESPACE="${NAMESPACE}"
    export HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
    export IQE_MARKER="${IQE_MARKER}"
    
    local test_result=0
    if ! "${iqe_script}" --namespace "${NAMESPACE}" --profile "${IQE_PROFILE}" --marker "${IQE_MARKER}"; then
        log_error "IQE tests failed"
        log_info "IQE JUnit report available at: tests/reports/iqe_junit.xml"
        log_info "IQE output log available at: tests/reports/iqe_output.log"
        test_result=1
    else
        log_success "IQE tests completed"
    fi
    
    return ${test_result}
}

################################################################################
# Version tracking
################################################################################

save_version_info() {
    log_step "Saving deployment version information"
    
    local check_components_script="${LOCAL_SCRIPTS_DIR}/qe/check-components.sh"
    
    if [[ ! -x "${check_components_script}" ]]; then
        log_warning "check-components.sh not found at: ${check_components_script}"
        log_warning "Skipping version info generation"
        return 0
    fi
    
    if [[ "${DRY_RUN}" == "true" ]]; then
        log_info "DRY RUN: Would execute: MODE=deployment-info NAMESPACE=${NAMESPACE} ${check_components_script}"
        log_info "DRY RUN: Would save version info to: ${VERSION_INFO_FILE}"
        return 0
    fi
    
    # Export environment variables for check-components.sh
    export NAMESPACE="${NAMESPACE}"
    export HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
    export VERSION_INFO_FILE="${VERSION_INFO_FILE}"
    export MODE="deployment-info"
    
    if "${check_components_script}"; then
        log_success "Version info saved to: ${VERSION_INFO_FILE}"
        
        # Display summary if verbose
        if [[ "${VERBOSE}" == "true" ]] && [[ -f "${VERSION_INFO_FILE}" ]]; then
            log_verbose "Version info contents:"
            cat "${VERSION_INFO_FILE}"
        fi
    else
        log_warning "Failed to generate version info"
    fi
}

################################################################################
# Main deployment workflow
################################################################################

print_summary() {
    echo ""

    # Show execution mode
    if [[ "${PERF_ONLY}" == "true" ]]; then
        log_info "Mode: Performance-only (--perf-only)"
    elif [[ "${TESTS_ONLY}" == "true" ]] && [[ "${SKIP_TEST}" == "true" ]] && [[ "${RUN_IQE}" == "true" ]]; then
        log_info "Mode: IQE-only (--iqe-only)"
    elif [[ "${TESTS_ONLY}" == "true" ]]; then
        log_info "Mode: Tests-only (--skip-deploy)"
    else
        log_info "Mode: Full deployment"
    fi

    log_info "Deployment Configuration:"
    echo "  Namespace:           ${NAMESPACE}"
    [[ "${DEPLOY_S4}" == "true" ]] && echo "  S4 Namespace:        ${S4_NAMESPACE}"
    [[ "${OPENSHIFT_VALUES_FILE}" != "openshift-values.yaml" ]] && echo "  Values File:         ${OPENSHIFT_VALUES_FILE}"
    [[ -n "${SIZING_PROFILE}" ]] && echo "  Sizing Profile:      ${SIZING_PROFILE}"
    echo "  Use Local Chart:     ${USE_LOCAL_CHART}"
    [[ "${USE_HELM_DEVEL}" == "true" ]] && echo "  Include Pre-release: ${USE_HELM_DEVEL}"
    [[ -n "${CHART_VERSION}" ]] && echo "  Chart Version:       ${CHART_VERSION}"
    echo ""
    log_info "Steps to execute:"
    [[ "${SKIP_RHBK}" == "false" ]] && echo "  ✓ Deploy Red Hat Build of Keycloak (RHBK)" || echo "  ✗ Deploy RHBK (SKIPPED)"
    [[ "${SKIP_KAFKA}" == "false" ]] && echo "  ✓ Deploy Kafka/AMQ Streams" || echo "  ✗ Deploy Kafka/AMQ Streams (SKIPPED)"
    [[ "${DEPLOY_S4}" == "true" ]] && echo "  ✓ Deploy S4 Storage (namespace: ${S4_NAMESPACE})" || echo "  ✗ Deploy S4 Storage (OPTIONAL)"
    [[ "${SKIP_HELM}" == "false" ]] && echo "  ✓ Deploy Cost On-Prem Helm Chart" || echo "  ✗ Deploy Cost On-Prem Helm Chart (SKIPPED)"
    [[ "${SKIP_TLS}" == "false" ]] && echo "  ✓ Setup TLS Certificates" || echo "  ✗ Setup TLS Certificates (SKIPPED)"
    [[ "${SKIP_TEST}" == "false" ]] && echo "  ✓ Run Chart Tests" || echo "  ✗ Run Chart Tests (SKIPPED)"
    if [[ "${DEPLOY_OBSERVABILITY}" == "true" ]]; then
        echo "  ✓ Deploy Observability (postgres_exporter, valkey-exporter, celery-exporter)"
    else
        echo "  ✗ Deploy Observability (OPTIONAL)"
    fi
    if [[ "${PERF_ONLY}" == "true" ]] || [[ "${RUN_PERF}" == "true" ]]; then
        local perf_opts="profile: ${PERF_PROFILE}"
        [[ "${PERF_SUITE}" != "all" ]] && perf_opts="${perf_opts}, suite: ${PERF_SUITE}"
        [[ "${SKIP_PROFILE_CONFIG}" == "true" ]] && perf_opts="${perf_opts}, skip-profile-config"
        [[ "${COLLECT_METRICS}" == "true" ]] && perf_opts="${perf_opts}, collect-metrics: ${METRICS_INTERVAL}s"
        [[ "${UPLOAD_METRICS}" == "true" ]] && perf_opts="${perf_opts}, upload-to-s3"
        echo "  ✓ Run Performance Tests (${perf_opts})"
    else
        echo "  ✗ Run Performance Tests (OPTIONAL)"
    fi
    if [[ "${RUN_IQE}" == "true" ]]; then
        local iqe_opts="profile: ${IQE_PROFILE}, marker: ${IQE_MARKER}"
        [[ -n "${LISTENER_CPU_LIMIT}" ]] && iqe_opts="${iqe_opts}, listener-cpu: ${LISTENER_CPU_LIMIT}"
        echo "  ✓ Run IQE Tests (${iqe_opts})"
    else
        echo "  ✗ Run IQE Tests (OPTIONAL)"
    fi
    echo ""
}

print_completion() {
    echo ""
    log_success "Deployment completed successfully"
    echo ""
    log_info "Cost On-Prem with JWT authentication deployed to namespace: ${NAMESPACE}"
    echo ""
    log_info "Next steps:"
    echo "  1. Verify: oc get pods -n ${NAMESPACE}"
    echo "  2. Check route: oc get route -n ${NAMESPACE}"
    echo "  3. View logs: oc logs -n ${NAMESPACE} -l app.kubernetes.io/component=ingress -f"
    echo ""
}

main() {
    echo ""
    echo -e "${CYAN}Cost On-Prem OpenShift with JWT Authentication Deployment${NC}"
    echo ""

    # Parse command line arguments
    local -a ORIGINAL_ARGS=("$@")
    while [[ $# -gt 0 ]]; do
        case $1 in
            --skip-rhbk)
                SKIP_RHBK=true
                shift
                ;;
            --skip-kafka)
                SKIP_KAFKA=true
                shift
                ;;
            --skip-helm)
                SKIP_HELM=true
                shift
                ;;
            --skip-tls)
                SKIP_TLS=true
                shift
                ;;
            --skip-test|--skip-chart-tests)
                SKIP_TEST=true
                shift
                ;;
            --deploy-s4)
                DEPLOY_S4=true
                shift
                ;;
            --s4-namespace)
                S4_NAMESPACE="$2"
                shift 2
                ;;
            --namespace)
                NAMESPACE="$2"
                shift 2
                ;;
            --image-tag)
                IMAGE_TAG="$2"
                shift 2
                ;;
            --use-local-chart)
                USE_LOCAL_CHART=true
                shift
                ;;
            --devel)
                USE_HELM_DEVEL=true
                shift
                ;;
            --chart-version)
                CHART_VERSION="$2"
                shift 2
                ;;
            --sizing-profile)
                SIZING_PROFILE="$2"
                shift 2
                ;;
            --verbose)
                VERBOSE=true
                shift
                ;;
            --dry-run)
                DRY_RUN=true
                shift
                ;;
            --tests-only|--skip-deploy)
                TESTS_ONLY=true
                shift
                ;;
            --include-ui)
                INCLUDE_UI=true
                shift
                ;;
            --run-perf)
                RUN_PERF=true
                shift
                ;;
            --perf-profile)
                PERF_PROFILE="$2"
                shift 2
                ;;
            --perf-suite)
                PERF_SUITE="$2"
                shift 2
                ;;
            --perf-only)
                PERF_ONLY=true
                SKIP_RHBK=true
                SKIP_KAFKA=true
                SKIP_HELM=true
                SKIP_TLS=true
                SKIP_TEST=true
                RUN_PERF=true
                shift
                ;;
            --deploy-observability)
                DEPLOY_OBSERVABILITY=true
                shift
                ;;
            --collect-metrics)
                COLLECT_METRICS=true
                shift
                ;;
            --upload-metrics)
                UPLOAD_METRICS=true
                shift
                ;;
            --metrics-interval)
                METRICS_INTERVAL="$2"
                shift 2
                ;;
            --run-iqe)
                RUN_IQE=true
                shift
                ;;
            --iqe-only)
                TESTS_ONLY=true
                SKIP_TEST=true
                RUN_IQE=true
                shift
                ;;
            --iqe-marker)
                IQE_MARKER="$2"
                shift 2
                ;;
            --iqe-profile)
                IQE_PROFILE="$2"
                shift 2
                ;;
            --listener-cpu)
                LISTENER_CPU_LIMIT="$2"
                shift 2
                ;;
            --skip-profile-config)
                SKIP_PROFILE_CONFIG=true
                shift
                ;;
            --save-versions)
                SAVE_VERSIONS=true
                # Check if next argument is a file path (not another flag)
                if [[ -n "${2:-}" ]] && [[ ! "$2" =~ ^-- ]]; then
                    VERSION_INFO_FILE="$2"
                    shift
                fi
                shift
                ;;
            --grafana-links)
                SKIP_GRAFANA_LINKS=false
                shift
                ;;
            --skip-grafana-links)
                SKIP_GRAFANA_LINKS=true
                shift
                ;;
            --help|-h)
                show_help
                ;;
            *)
                log_error "Unknown option: $1"
                echo "Use --help for usage information"
                exit 1
                ;;
        esac
    done

    # Validate flag combinations
    if [[ "${USE_LOCAL_CHART}" == "true" ]]; then
        if [[ "${USE_HELM_DEVEL}" == "true" ]]; then
            log_error "Cannot use --devel with --use-local-chart"
            exit 1
        fi
        if [[ -n "${CHART_VERSION}" ]]; then
            log_error "Cannot use --chart-version with --use-local-chart"
            exit 1
        fi
    fi

    # Validate --perf-suite values
    if [[ "${PERF_SUITE}" != "all" ]]; then
        IFS=',' read -ra _suites <<< "${PERF_SUITE}"
        for _s in "${_suites[@]}"; do
            case "${_s}" in
                api|ros|ingestion|scale|soak|valkey|db|kafka|celery|stress|stress_ramp|stress_recovery) ;;
                *) log_error "Invalid --perf-suite value: ${_s} (valid: all, api, ros, ingestion, scale, soak, valkey, db, kafka, celery, stress, stress_ramp, stress_recovery)"
                   exit 1 ;;
            esac
        done
    fi

    # When --sizing-profile is set, the deploy-time overlay already configures
    # resources/replicas/timeouts.  Auto-enable --skip-profile-config so
    # apply_perf_profile_config() doesn't overwrite at test time — unless the
    # user explicitly passed --perf-profile to override.
    if [[ -n "${SIZING_PROFILE}" ]] && [[ "${SKIP_PROFILE_CONFIG}" == "false" ]]; then
        # Detect whether --perf-profile was explicitly passed (vs the default)
        local perf_profile_explicit=false
        for _arg in "${ORIGINAL_ARGS[@]:-}"; do
            [[ "${_arg}" == "--perf-profile" ]] && perf_profile_explicit=true
        done
        if [[ "${perf_profile_explicit}" == "false" ]]; then
            SKIP_PROFILE_CONFIG=true
            log_info "Auto-enabled --skip-profile-config (--sizing-profile ${SIZING_PROFILE} sets resources at deploy time)"
        else
            log_warning "--sizing-profile ${SIZING_PROFILE} and --perf-profile ${PERF_PROFILE} both set; perf profile will override sizing at test time"
        fi
    fi

    # In tests-only / skip-deploy mode, skip all deployment steps
    if [[ "${TESTS_ONLY}" == "true" ]]; then
        SKIP_RHBK=true
        SKIP_KAFKA=true
        SKIP_HELM=true
        SKIP_TLS=true
    fi

    # Show deployment summary
    print_summary

    if [[ "${DRY_RUN}" == "true" ]]; then
        log_warning "DRY RUN MODE: No changes will be made"
        echo ""
    fi

    # Execute deployment steps
    check_prerequisites
    check_oc_connection
    if [[ "${TESTS_ONLY}" != "true" ]]; then
        create_namespace
    fi

    deploy_rhbk
    deploy_kafka

    # Save the user-supplied S3_ENDPOINT before deploy_s4 overwrites it
    # with an in-cluster S4 endpoint for chart installation.  After the
    # Helm chart is installed we restore the original so that downstream
    # steps (metrics upload, etc.) target the external endpoint.
    local _user_s3_endpoint="${S3_ENDPOINT:-}"
    local _user_s3_port="${S3_PORT:-}"
    local _user_s3_use_ssl="${S3_USE_SSL:-}"

    deploy_s4

    deploy_helm_chart
    setup_tls

    # Restore the original S3 env so perf-upload targets the external endpoint
    if [[ -n "${_user_s3_endpoint}" ]]; then
        export S3_ENDPOINT="${_user_s3_endpoint}"
        export S3_PORT="${_user_s3_port}"
        export S3_USE_SSL="${_user_s3_use_ssl}"
    fi
    
    # Deploy observability after Helm chart so services exist for auto-detection
    if [[ "${DEPLOY_OBSERVABILITY}" == "true" ]]; then
        if type deploy_observability &>/dev/null; then
            deploy_observability
        else
            log_warning "--deploy-observability requested but perf-observability.sh not found; skipping"
        fi
    fi
    
    # Run tests and capture result (don't exit on failure)
    local test_result=0
    run_tests || test_result=$?

    # Save version information if requested
    if [[ "${SAVE_VERSIONS}" == "true" ]]; then
        save_version_info
    fi

    # Print completion message
    if [[ "${DRY_RUN}" == "false" ]]; then
        if [[ "${test_result}" -eq 0 ]]; then
            print_completion
        else
            echo ""
            log_error "Deployment completed but tests failed (exit code: ${test_result})"
            echo ""
        fi
    else
        echo ""
        log_info "DRY RUN completed. No changes were made."
        echo ""
    fi

    exit ${test_result}
}

# Run main function
main "$@"