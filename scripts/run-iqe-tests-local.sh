#!/bin/bash
# Run IQE cost-management tests locally against a deployed cost-onprem chart
#
# This script runs tests directly from local iqe-core and iqe-cost-management-plugin
# repositories, providing more control over dependencies and faster iteration.
#
# Usage:
#   ./scripts/run-iqe-tests-local.sh [OPTIONS]
#
# Prerequisites:
#   - Python 3.12+
#   - Access to Red Hat internal PyPI (nexus.corp.redhat.com)
#   - Local clones of iqe-core and iqe-cost-management-plugin
#   - Logged into OpenShift cluster with cost-onprem deployed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Source shared filter configuration
# shellcheck source=lib/iqe-filters.sh
source "${SCRIPT_DIR}/lib/iqe-filters.sh"

# Defaults
NAMESPACE="${NAMESPACE:-cost-onprem}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
IQE_MARKER="${IQE_MARKER:-cost_ocp_on_prem}"
KEYCLOAK_NS="${KEYCLOAK_NS:-keycloak}"
KEYCLOAK_SECRET_NAME="${KEYCLOAK_SECRET_NAME:-keycloak-client-secret-cost-management-operator}"
# Local repository paths (can be overridden via env vars)
IQE_CORE_PATH="${IQE_CORE_PATH:-${PROJECT_ROOT}/../iqe-core}"
IQE_PLUGIN_PATH="${IQE_PLUGIN_PATH:-${PROJECT_ROOT}/../iqe-cost-management-plugin}"
VENV_PATH="${VENV_PATH:-${PROJECT_ROOT}/.venv-iqe}"

# PyPI configuration for Red Hat internal packages
PYPI_INDEX_URL="${PYPI_INDEX_URL:-https://nexus.corp.redhat.com/repository/cqt-pypi/simple}"

# Feature flags
DO_SETUP=false
CLEAN_SOURCES=false
NISE_VERSION=""
VERBOSE=false
DRY_RUN=false
PYTHON_BIN="${PYTHON_BIN:-python3.12}"

# Masu route tracking
MASU_ROUTE_CREATED=false
MASU_ROUTE_NAME="${HELM_RELEASE_NAME:-cost-onprem}-masu-iqe"

show_help() {
    cat << EOF
Run IQE cost-management tests locally against a deployed cost-onprem chart

This script runs tests directly from local iqe-core and iqe-cost-management-plugin
repositories instead of using the container image. This provides:
  - Control over koku-nise version (useful for GPU/MIG schema issues)
  - Faster iteration (edit tests and re-run immediately)
  - Full debugging capability with local IDE

Usage: $(basename "$0") [OPTIONS]

Options:
    --setup              Create/update virtual environment and install dependencies.
                         NOTE: When --setup is used, the script exits after setup
                         completes without running tests. Run again without --setup
                         to execute tests.
    --namespace NAME     Target namespace (default: cost-onprem)
    --marker EXPR        Pytest marker expression (default: cost_ocp_on_prem)
    --filter EXPR        Pytest -k filter expression to select/deselect tests
    --profile PROFILE    Test profile (smoke, extended, stable, full)
    --nise-version VER   Override koku-nise version (e.g., 5.2.0 for pre-MIG)
    --clean-sources      Delete all sources before running tests
    --verbose            Enable verbose output
    --dry-run            Show what would be done without executing
    --help               Show this help message

Test Profiles (use --profile):
    smoke      Source + cost model tests (~43 tests, ~17 min) - PR checks
    extended   All except infra tests (~2100 tests, ~33 min) - Daily CI
    stable     All validated tests (~2350 tests, ~40 min) - Weekly CI
    full       All cost_ocp_on_prem tests (~3324 tests, ~60 min) - Release
    (default)  Same as stable

Environment Variables:
    IQE_CORE_PATH        Path to iqe-core repo (default: ../iqe-core)
    IQE_PLUGIN_PATH      Path to iqe-cost-management-plugin repo (default: ../iqe-cost-management-plugin)
    VENV_PATH            Path to virtual environment (default: .venv-iqe)
    PYTHON_BIN           Python binary to use (default: python3.12)
    PYPI_INDEX_URL       PyPI index URL for Red Hat packages

Examples:
    # First time setup
    ./scripts/run-iqe-tests-local.sh --setup

    # Quick smoke tests for PR validation (~17 min)
    ./scripts/run-iqe-tests-local.sh --profile smoke

    # Extended tests for daily CI
    ./scripts/run-iqe-tests-local.sh --profile extended

    # Run with custom filter
    ./scripts/run-iqe-tests-local.sh --filter "test_api_ocp_source"

    # Use older NISE version without MIG support
    ./scripts/run-iqe-tests-local.sh --setup --nise-version 5.2.0

    # Dry run to see configuration
    ./scripts/run-iqe-tests-local.sh --dry-run
EOF
}

log() {
    echo "[$(date '+%H:%M:%S')] $*"
}

log_verbose() {
    if [ "$VERBOSE" = true ]; then
        log "$@"
    fi
}

error() {
    echo "[ERROR] $*" >&2
}

cleanup() {
    log "Cleaning up..."
    if [ "$MASU_ROUTE_CREATED" = true ]; then
        log "Removing masu route ($MASU_ROUTE_NAME)"
        kubectl delete route "$MASU_ROUTE_NAME" -n "$NAMESPACE" 2>/dev/null || true
    fi
    rm -f /tmp/iqe-ca-bundle-*.crt 2>/dev/null || true
}

trap cleanup EXIT

# Parse arguments
EXPLICIT_FILTER=""
while [[ $# -gt 0 ]]; do
    case $1 in
        --setup) DO_SETUP=true; shift ;;
        --namespace) NAMESPACE="$2"; shift 2 ;;
        --marker) IQE_MARKER="$2"; shift 2 ;;
        --filter) EXPLICIT_FILTER="$2"; shift 2 ;;
        --profile) TEST_PROFILE="$2"; shift 2 ;;
        --nise-version) NISE_VERSION="$2"; shift 2 ;;
        --clean-sources) CLEAN_SOURCES=true; shift ;;
        --include-slow) SKIP_SLOW_TESTS=false; shift ;;
        --skip-slow) SKIP_SLOW_TESTS=true; shift ;;
        --verbose) VERBOSE=true; shift ;;
        --dry-run) DRY_RUN=true; shift ;;
        --help) show_help; exit 0 ;;
        *) error "Unknown option: $1"; show_help; exit 1 ;;
    esac
done

# Apply profile settings if specified
if [[ -n "${TEST_PROFILE}" ]]; then
    apply_profile
fi

# Build filter after argument parsing
if [[ "${SKIP_FILTER_BUILD:-false}" == "true" ]]; then
    IQE_FILTER=""
elif [[ -n "${EXPLICIT_FILTER}" ]]; then
    IQE_FILTER="${EXPLICIT_FILTER}"
elif [[ -n "${SMOKE_FILTER:-}" ]]; then
    SKIP_FILTER=$(build_test_filter)
    if [[ -n "${SKIP_FILTER}" ]]; then
        IQE_FILTER="(${SMOKE_FILTER}) and ${SKIP_FILTER}"
    else
        IQE_FILTER="${SMOKE_FILTER}"
    fi
else
    IQE_FILTER=$(build_test_filter)
fi

# =============================================================================
# Validation
# =============================================================================

validate_prerequisites() {
    log "Validating prerequisites..."
    
    # Check Python version
    if ! command -v "$PYTHON_BIN" &>/dev/null; then
        error "Python binary not found: $PYTHON_BIN"
        error "Please install Python 3.12+ or set PYTHON_BIN environment variable"
        exit 1
    fi
    
    local python_version
    python_version=$("$PYTHON_BIN" -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')
    log_verbose "Python version: $python_version"
    
    if [[ ! "$python_version" =~ ^3\.(1[2-9]|[2-9][0-9])$ ]]; then
        error "Python 3.12+ required, found: $python_version"
        exit 1
    fi
    
    # Check iqe-core repo
    if [ ! -d "$IQE_CORE_PATH" ]; then
        error "iqe-core repository not found at: $IQE_CORE_PATH"
        error "Please clone it or set IQE_CORE_PATH environment variable"
        error "  git clone https://gitlab.cee.redhat.com/insights-qe/iqe-core.git $IQE_CORE_PATH"
        exit 1
    fi
    
    if [ ! -f "$IQE_CORE_PATH/pyproject.toml" ]; then
        error "Invalid iqe-core repository (missing pyproject.toml): $IQE_CORE_PATH"
        exit 1
    fi
    
    # Check iqe-cost-management-plugin repo
    if [ ! -d "$IQE_PLUGIN_PATH" ]; then
        error "iqe-cost-management-plugin repository not found at: $IQE_PLUGIN_PATH"
        error "Please clone it or set IQE_PLUGIN_PATH environment variable"
        error "  git clone https://gitlab.cee.redhat.com/insights-qe/iqe-cost-management-plugin.git $IQE_PLUGIN_PATH"
        exit 1
    fi
    
    if [ ! -f "$IQE_PLUGIN_PATH/pyproject.toml" ]; then
        error "Invalid iqe-cost-management-plugin repository (missing pyproject.toml): $IQE_PLUGIN_PATH"
        exit 1
    fi
    
    # Check kubectl/oc
    if ! command -v kubectl &>/dev/null && ! command -v oc &>/dev/null; then
        error "kubectl or oc command not found"
        exit 1
    fi
    
    # Check cluster connectivity
    if ! kubectl cluster-info &>/dev/null; then
        error "Not connected to a Kubernetes cluster"
        error "Please run: oc login <cluster-url>"
        exit 1
    fi
    
    log "✓ Prerequisites validated"
}

# =============================================================================
# Virtual Environment Setup
# =============================================================================

setup_venv() {
    log "Setting up virtual environment at: $VENV_PATH"
    
    if [ "$DRY_RUN" = true ]; then
        log "[DRY RUN] Would create venv and install dependencies"
        return
    fi
    
    # Create venv if it doesn't exist
    if [ ! -d "$VENV_PATH" ]; then
        log "Creating virtual environment..."
        "$PYTHON_BIN" -m venv "$VENV_PATH"
    fi
    
    # Activate venv
    # shellcheck source=/dev/null
    source "$VENV_PATH/bin/activate"
    
    # Upgrade pip
    log "Upgrading pip..."
    pip install --upgrade pip --quiet
    
    # Check if uv is available for faster installs
    local use_uv=false
    if command -v uv &>/dev/null; then
        use_uv=true
        log "Using uv for faster package installation"
    fi
    
    # Configure PyPI index
    export PIP_INDEX_URL="$PYPI_INDEX_URL"
    export UV_INDEX_URL="$PYPI_INDEX_URL"
    
    # Install iqe-core in editable mode
    log "Installing iqe-core (editable)..."
    if [ "$use_uv" = true ]; then
        uv pip install -e "$IQE_CORE_PATH"
    else
        pip install -e "$IQE_CORE_PATH"
    fi
    
    # Install iqe-cost-management-plugin in editable mode
    log "Installing iqe-cost-management-plugin (editable)..."
    if [ "$use_uv" = true ]; then
        uv pip install -e "$IQE_PLUGIN_PATH"
    else
        pip install -e "$IQE_PLUGIN_PATH"
    fi
    
    # Override NISE version if specified
    if [ -n "$NISE_VERSION" ]; then
        log "Installing koku-nise version: $NISE_VERSION"
        if [ "$use_uv" = true ]; then
            uv pip install "koku-nise==$NISE_VERSION"
        else
            pip install "koku-nise==$NISE_VERSION"
        fi
    fi
    
    # Verify installation
    log "Verifying installation..."
    if ! python -c "import iqe; import iqe_cost_management" 2>/dev/null; then
        error "Failed to import IQE modules"
        exit 1
    fi
    
    local nise_ver
    nise_ver=$(pip show koku-nise 2>/dev/null | grep "^Version:" | cut -d' ' -f2)
    log "✓ Virtual environment ready"
    log "  - iqe-core: installed (editable)"
    log "  - iqe-cost-management-plugin: installed (editable)"
    log "  - koku-nise: $nise_ver"
}

activate_venv() {
    if [ ! -d "$VENV_PATH" ]; then
        error "Virtual environment not found at: $VENV_PATH"
        error "Run with --setup first: ./scripts/run-iqe-tests-local.sh --setup"
        exit 1
    fi
    
    # shellcheck source=/dev/null
    source "$VENV_PATH/bin/activate"
    log_verbose "Activated virtual environment: $VENV_PATH"
}

# =============================================================================
# Cluster Configuration Extraction
# =============================================================================

extract_cluster_config() {
    log "Extracting cluster configuration..."
    
    # Get Keycloak client credentials
    if ! kubectl get secret "$KEYCLOAK_SECRET_NAME" -n "$KEYCLOAK_NS" &>/dev/null; then
        error "Keycloak secret not found: $KEYCLOAK_SECRET_NAME in namespace $KEYCLOAK_NS"
        exit 1
    fi
    
    export DYNACONF_ONPREM_CLIENT_ID
    export DYNACONF_ONPREM_CLIENT_SECRET
    DYNACONF_ONPREM_CLIENT_ID=$(kubectl get secret "$KEYCLOAK_SECRET_NAME" -n "$KEYCLOAK_NS" \
        -o jsonpath='{.data.CLIENT_ID}' | base64 -d)
    DYNACONF_ONPREM_CLIENT_SECRET=$(kubectl get secret "$KEYCLOAK_SECRET_NAME" -n "$KEYCLOAK_NS" \
        -o jsonpath='{.data.CLIENT_SECRET}' | base64 -d)
    
    if [ -z "$DYNACONF_ONPREM_CLIENT_ID" ] || [ -z "$DYNACONF_ONPREM_CLIENT_SECRET" ]; then
        error "Failed to extract Keycloak credentials"
        exit 1
    fi
    
    # Get Koku API hostname from route
    export DYNACONF_ONPREM_KOKU_HOSTNAME
    DYNACONF_ONPREM_KOKU_HOSTNAME=$(kubectl get route "${HELM_RELEASE_NAME}-api" -n "$NAMESPACE" \
        -o jsonpath='{.spec.host}' 2>/dev/null || \
        kubectl get route "${HELM_RELEASE_NAME}-gateway" -n "$NAMESPACE" \
        -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
    
    if [ -z "$DYNACONF_ONPREM_KOKU_HOSTNAME" ]; then
        error "Failed to get Koku API route"
        error "Tried: ${HELM_RELEASE_NAME}-api, ${HELM_RELEASE_NAME}-gateway"
        exit 1
    fi
    
    # Get Keycloak OAuth URL
    local keycloak_host
    keycloak_host=$(kubectl get route keycloak -n "$KEYCLOAK_NS" -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
    if [ -z "$keycloak_host" ]; then
        error "Failed to get Keycloak route"
        exit 1
    fi
    export DYNACONF_ONPREM_OAUTH_URL="https://${keycloak_host}/realms/kubernetes/protocol/openid-connect"
    
    # Masu configuration — set after ensure_masu_route populates MASU_ROUTE_HOST
    export DYNACONF_ONPREM_MASU_HOSTNAME="${MASU_ROUTE_HOST:-pending}"
    export DYNACONF_ONPREM_MASU_PORT=""
    export DYNACONF_ONPREM_MASU_SCHEME="https"
    
    # S3/object storage configuration (used by cost_minio_settings fixture for ROS tests).
    # Fetch all needed env vars from masu in a single kubectl exec to avoid repeated pod calls.
    local masu_deploy="deploy/${HELM_RELEASE_NAME}-koku-masu"
    local masu_env
    masu_env=$(kubectl exec -n "$NAMESPACE" "$masu_deploy" -c masu -- env 2>/dev/null \
        | grep -E '^(S3_ENDPOINT|REQUESTED_BUCKET|REQUESTED_ROS_BUCKET)=' || true)

    local s3_internal_endpoint
    s3_internal_endpoint=$(echo "$masu_env" | grep '^S3_ENDPOINT=' | cut -d= -f2- || true)
    export S3_KOKU_BUCKET
    S3_KOKU_BUCKET=$(echo "$masu_env" | grep '^REQUESTED_BUCKET=' | cut -d= -f2-)
    S3_KOKU_BUCKET="${S3_KOKU_BUCKET:-koku-bucket}"
    export S3_ROS_BUCKET
    S3_ROS_BUCKET=$(echo "$masu_env" | grep '^REQUESTED_ROS_BUCKET=' | cut -d= -f2-)
    S3_ROS_BUCKET="${S3_ROS_BUCKET:-ros-data}"
    export S3_SECRET_NAME="${HELM_RELEASE_NAME}-storage-credentials"

    # The masu pod uses in-cluster DNS (e.g. https://s3.openshift-storage.svc).
    # For local runs we need the external route instead.
    if [[ "$s3_internal_endpoint" =~ \.svc(/|$|:) ]] || [[ "$s3_internal_endpoint" =~ \.svc\.cluster ]]; then
        local s3_host s3_svc_name s3_ns s3_route_host
        s3_host=$(echo "$s3_internal_endpoint" | sed -E 's|https?://||; s|[:/].*||')
        s3_svc_name="${s3_host%%.*}"
        s3_ns="${s3_host#*.}"
        s3_ns="${s3_ns%.svc*}"

        s3_route_host=$(kubectl get route "$s3_svc_name" -n "$s3_ns" \
            -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
        if [ -n "$s3_route_host" ]; then
            export S3_ENDPOINT="https://$s3_route_host"
            log_verbose "  S3: resolved in-cluster $s3_internal_endpoint → external $S3_ENDPOINT"
        else
            export S3_ENDPOINT="$s3_internal_endpoint"
            log "WARNING: S3 endpoint $s3_internal_endpoint is in-cluster but no external route found"
        fi
    else
        export S3_ENDPOINT="$s3_internal_endpoint"
    fi

    if [[ "$S3_ENDPOINT" =~ ^http:// ]]; then
        export S3_USE_SSL="false"
    else
        export S3_USE_SSL="true"
    fi
    if [[ "$S3_ENDPOINT" =~ :([0-9]+)/?$ ]]; then
        export S3_PORT="${BASH_REMATCH[1]}"
    else
        export S3_PORT="443"
    fi

    if [ -n "$S3_ENDPOINT" ]; then
        log "  S3: ${S3_ENDPOINT} (buckets: koku=${S3_KOKU_BUCKET}, ros=${S3_ROS_BUCKET})"
    else
        log "WARNING: S3_ENDPOINT not found on MASU deployment — ROS tests will skip S3 operations"
    fi

    
    # Direct target values - bypass Jinja templates that don't evaluate correctly
    # These match what the containerized tests use in run-iqe-tests.sh
    export DYNACONF_MAIN__HOSTNAME="$DYNACONF_ONPREM_KOKU_HOSTNAME"
    export DYNACONF_MAIN__SCHEME="https"
    export DYNACONF_MAIN__SSL_VERIFY="false"
    export DYNACONF_HTTP__DEFAULT_AUTH_TYPE="jwt-auth"
    export DYNACONF_HTTP__OAUTH_CLIENT_ID="$DYNACONF_ONPREM_CLIENT_ID"
    export DYNACONF_HTTP__OAUTH_BASE_URL="$DYNACONF_ONPREM_OAUTH_URL"
    export DYNACONF_HTTP__SSL_VERIFY="false"
    
    # Service objects configuration
    export DYNACONF_SERVICE_OBJECTS__KOKU__CONFIG__HOSTNAME="$DYNACONF_ONPREM_KOKU_HOSTNAME"
    export DYNACONF_SERVICE_OBJECTS__KOKU__CONFIG__SCHEME="https"
    export DYNACONF_SERVICE_OBJECTS__KOKU__CONFIG__PORT=""
    export DYNACONF_SERVICE_OBJECTS__MASU__CONFIG__HOSTNAME="$DYNACONF_ONPREM_MASU_HOSTNAME"
    export DYNACONF_SERVICE_OBJECTS__MASU__CONFIG__PORT=""
    export DYNACONF_SERVICE_OBJECTS__MASU__CONFIG__SCHEME="https"
    export DYNACONF_SERVICE_OBJECTS__COST_MANAGEMENT_SOURCES__CONFIG__HOSTNAME="$DYNACONF_ONPREM_KOKU_HOSTNAME"
    export DYNACONF_SERVICE_OBJECTS__COST_MANAGEMENT_SOURCES__CONFIG__SCHEME="https"
    export DYNACONF_SERVICE_OBJECTS__COST_MANAGEMENT_SOURCES__CONFIG__PORT=""
    
    # User configuration
    # IMPORTANT: Use lowercase for nested keys (auth, identity) because IQE code
    # expects lowercase keys like app_user["auth"], not app_user["AUTH"]
    export DYNACONF_DEFAULT_USER="cost_onprem_user"
    export DYNACONF_users__cost_onprem_user__auth__username="test"
    export DYNACONF_users__cost_onprem_user__auth__password="test"
    export DYNACONF_users__cost_onprem_user__auth__jwt_grant_type="client_credentials"
    export DYNACONF_users__cost_onprem_user__auth__client_id="$DYNACONF_ONPREM_CLIENT_ID"
    export DYNACONF_users__cost_onprem_user__auth__client_secret="$DYNACONF_ONPREM_CLIENT_SECRET"
    # Read org_id and account_number from Keycloak (canonical source: jwtAuth.realmUsers)
    local iqe_org_id="1234567"
    local iqe_acct="7890123"
    local kc_host
    kc_host=$(kubectl get route keycloak -n "$KEYCLOAK_NS" -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
    if [ -n "$kc_host" ]; then
        local kc_admin_user kc_admin_pass kc_token kc_user_json
        kc_admin_user=$(kubectl get secret keycloak-initial-admin -n "$KEYCLOAK_NS" -o jsonpath='{.data.username}' 2>/dev/null | base64 -d || echo "admin")
        kc_admin_pass=$(kubectl get secret keycloak-initial-admin -n "$KEYCLOAK_NS" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d || echo "")
        if [ -n "$kc_admin_pass" ]; then
            kc_token=$(curl -sk -X POST "https://${kc_host}/realms/master/protocol/openid-connect/token" \
                -d "client_id=admin-cli&grant_type=password&username=${kc_admin_user}&password=${kc_admin_pass}" 2>/dev/null | jq -r '.access_token // empty')
            if [ -n "$kc_token" ]; then
                # Find the first user with the org-admin realm role
                local admin_username="admin"
                local role_members
                role_members=$(curl -sk "https://${kc_host}/admin/realms/kubernetes/roles/org-admin/users" \
                    -H "Authorization: Bearer ${kc_token}" 2>/dev/null)
                local detected_admin
                detected_admin=$(echo "$role_members" | jq -r '.[0].username // empty')
                [ -n "$detected_admin" ] && admin_username="$detected_admin"

                kc_user_json=$(curl -sk "https://${kc_host}/admin/realms/kubernetes/users?username=${admin_username}&exact=true" \
                    -H "Authorization: Bearer ${kc_token}" 2>/dev/null)
                local detected_org detected_acct
                detected_org=$(echo "$kc_user_json" | jq -r '.[0].attributes.org_id[0] // empty')
                detected_acct=$(echo "$kc_user_json" | jq -r '.[0].attributes.account_number[0] // empty')
                [ -n "$detected_org" ] && iqe_org_id="$detected_org"
                [ -n "$detected_acct" ] && iqe_acct="$detected_acct"
            fi
        fi
    fi
    export DYNACONF_users__cost_onprem_user__identity__account_number="$iqe_acct"
    export DYNACONF_users__cost_onprem_user__identity__org_id="$iqe_org_id"
    
    if [ "$DRY_RUN" = true ]; then
        log "[DRY RUN] Cluster configuration:"
        log "  DYNACONF_ONPREM_CLIENT_ID: $DYNACONF_ONPREM_CLIENT_ID"
        log "  DYNACONF_ONPREM_CLIENT_SECRET: [HIDDEN]"
        log "  DYNACONF_ONPREM_KOKU_HOSTNAME: $DYNACONF_ONPREM_KOKU_HOSTNAME"
        log "  DYNACONF_ONPREM_OAUTH_URL: $DYNACONF_ONPREM_OAUTH_URL"
        log "  DYNACONF_ONPREM_MASU_HOSTNAME: $DYNACONF_ONPREM_MASU_HOSTNAME"
        log "  S3_ENDPOINT: ${S3_ENDPOINT:-<not set>}"
        log "  S3_KOKU_BUCKET: ${S3_KOKU_BUCKET:-<not set>}"
        log "  S3_ROS_BUCKET: ${S3_ROS_BUCKET:-<not set>}"
    fi
    
    log "✓ Cluster configuration extracted"
    log "  - Koku API: https://$DYNACONF_ONPREM_KOKU_HOSTNAME"
    log "  - Keycloak: $DYNACONF_ONPREM_OAUTH_URL"
    log "  - Masu: https://$DYNACONF_ONPREM_MASU_HOSTNAME (via route)"
    log "  - S3: ${S3_ENDPOINT:-<not configured>} (ros=${S3_ROS_BUCKET:-n/a})"
}

# =============================================================================
# SSL Certificate Setup
# =============================================================================

setup_ssl_certs() {
    log "Setting up SSL certificates..."
    
    local ca_bundle_file="/tmp/iqe-ca-bundle-$$.crt"
    
    if [ "$DRY_RUN" = true ]; then
        log "[DRY RUN] Would extract and configure cluster CA certificates"
        return
    fi
    
    # Extract ingress CA certificate
    local ingress_ca=""
    if kubectl get secret router-ca -n openshift-ingress-operator &>/dev/null; then
        ingress_ca=$(kubectl get secret router-ca -n openshift-ingress-operator \
            -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d || echo "")
    fi
    
    # Extract service CA certificate
    local service_ca=""
    if kubectl get configmap openshift-service-ca.crt -n openshift-config-managed &>/dev/null; then
        service_ca=$(kubectl get configmap openshift-service-ca.crt -n openshift-config-managed \
            -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null || echo "")
    fi
    
    # Combine certificates
    {
        if [ -n "$ingress_ca" ]; then
            echo "# OpenShift Ingress CA"
            echo "$ingress_ca"
        fi
        if [ -n "$service_ca" ]; then
            echo "# OpenShift Service CA"
            echo "$service_ca"
        fi
        # Include system CA bundle if available
        if [ -f /etc/pki/tls/certs/ca-bundle.crt ]; then
            echo "# System CA Bundle"
            cat /etc/pki/tls/certs/ca-bundle.crt
        elif [ -f /etc/ssl/certs/ca-certificates.crt ]; then
            echo "# System CA Bundle"
            cat /etc/ssl/certs/ca-certificates.crt
        fi
    } > "$ca_bundle_file"
    
    # Set environment variables for SSL
    export REQUESTS_CA_BUNDLE="$ca_bundle_file"
    export SSL_CERT_FILE="$ca_bundle_file"
    export CURL_CA_BUNDLE="$ca_bundle_file"
    
    log "✓ SSL certificates configured"
    log_verbose "  CA bundle: $ca_bundle_file"
}

# =============================================================================
# Masu Route Management
# =============================================================================

resolve_masu_service() {
    for svc_name in "${HELM_RELEASE_NAME}-koku-masu" "${HELM_RELEASE_NAME}-masu" "koku-masu" "masu"; do
        if kubectl get svc "$svc_name" -n "$NAMESPACE" &>/dev/null; then
            echo "$svc_name"
            return
        fi
    done
    error "Masu service not found in namespace $NAMESPACE"
    error "Available services:"
    kubectl get svc -n "$NAMESPACE" | grep -i masu || echo "  (none matching 'masu')"
    exit 1
}

ensure_masu_route() {
    log "Ensuring masu route exists..."

    if [ "$DRY_RUN" = true ]; then
        log "[DRY RUN] Would create edge-terminated route for masu service"
        return
    fi

    local masu_svc
    masu_svc=$(resolve_masu_service)

    local masu_host
    masu_host=$(kubectl get route "$MASU_ROUTE_NAME" -n "$NAMESPACE" \
        -o jsonpath='{.spec.host}' 2>/dev/null || echo "")

    if [ -n "$masu_host" ]; then
        log "Masu route already exists: https://$masu_host"
    else
        kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: $MASU_ROUTE_NAME
spec:
  to:
    kind: Service
    name: $masu_svc
  port:
    targetPort: 8000
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Redirect
EOF
        MASU_ROUTE_CREATED=true

        masu_host=$(kubectl get route "$MASU_ROUTE_NAME" -n "$NAMESPACE" \
            -o jsonpath='{.spec.host}')
        log "Created masu route: https://$masu_host"
    fi

    export MASU_ROUTE_HOST="$masu_host"

    # Verify the route hostname is resolvable. QE lab clusters often lack
    # wildcard DNS, relying on /etc/hosts entries for known routes.
    if ! grep -qw "$masu_host" /etc/hosts 2>/dev/null && ! host "$masu_host" &>/dev/null; then
        local router_ip
        router_ip=$(grep "apps.${masu_host#*apps.}" /etc/hosts 2>/dev/null \
            | head -1 | awk '{print $1}')
        if [ -n "$router_ip" ]; then
            error "Cannot resolve $masu_host"
            error "Add to /etc/hosts:  $router_ip $masu_host"
        else
            error "Cannot resolve $masu_host and no existing /etc/hosts entries found for this cluster"
        fi
        exit 1
    fi

    local retries=15
    while [ $retries -gt 0 ]; do
        if curl -skf -o /dev/null "https://$masu_host/api/cost-management/v1/status/" 2>/dev/null; then
            log "✓ Masu route ready (https://$masu_host -> $masu_svc:8000)"
            return
        fi
        sleep 1
        ((retries--))
    done

    error "Masu route created but not responding after 15s"
    exit 1
}

# =============================================================================
# Source Cleanup
# =============================================================================

clean_sources() {
    if [ "$CLEAN_SOURCES" != true ]; then
        return
    fi
    
    log "Cleaning up existing sources..."
    
    if [ "$DRY_RUN" = true ]; then
        log "[DRY RUN] Would delete all sources via API"
        return
    fi
    
    # Get OAuth token
    local token_url="${DYNACONF_ONPREM_OAUTH_URL}/token"
    local token_response
    token_response=$(curl -sk -X POST "$token_url" \
        -H "Content-Type: application/x-www-form-urlencoded" \
        -d "grant_type=client_credentials" \
        -d "client_id=${DYNACONF_ONPREM_CLIENT_ID}" \
        -d "client_secret=${DYNACONF_ONPREM_CLIENT_SECRET}" 2>/dev/null)
    
    local access_token
    access_token=$(echo "$token_response" | jq -r '.access_token // empty')
    
    if [ -z "$access_token" ]; then
        error "Failed to get OAuth token for cleanup"
        return 1
    fi
    
    # Get list of sources
    local api_url="https://${DYNACONF_ONPREM_KOKU_HOSTNAME}/api/cost-management/v1/sources/"
    local sources_response
    sources_response=$(curl -sk -X GET "$api_url" \
        -H "Authorization: Bearer ${access_token}" \
        -H "Content-Type: application/json" 2>/dev/null)
    
    local source_count
    source_count=$(echo "$sources_response" | jq -r '.meta.count // 0')
    
    if [ "$source_count" -eq 0 ]; then
        log "No sources to clean up"
        return
    fi
    
    log "Found $source_count sources to delete"
    
    # Delete each source
    local deleted=0
    for uuid in $(echo "$sources_response" | jq -r '.data[].uuid // empty'); do
        log_verbose "Deleting source: $uuid"
        local delete_response
        delete_response=$(curl -sk -X DELETE "${api_url}${uuid}/" \
            -H "Authorization: Bearer ${access_token}" \
            -H "Content-Type: application/json" \
            -w "%{http_code}" -o /dev/null 2>/dev/null)
        
        if [ "$delete_response" = "204" ] || [ "$delete_response" = "200" ]; then
            ((deleted++))
        else
            log_verbose "Warning: Failed to delete source $uuid (HTTP $delete_response)"
        fi
    done
    
    log "✓ Deleted $deleted sources"
    
    # Also clean up cost models
    local cost_models_url="https://${DYNACONF_ONPREM_KOKU_HOSTNAME}/api/cost-management/v1/cost-models/"
    local cost_models_response
    cost_models_response=$(curl -sk -X GET "$cost_models_url?limit=500" \
        -H "Authorization: Bearer ${access_token}" \
        -H "Content-Type: application/json" 2>/dev/null)
    
    local cm_count
    cm_count=$(echo "$cost_models_response" | jq -r '.meta.count // 0')
    
    if [ "$cm_count" -gt 0 ]; then
        log "Found $cm_count cost models to delete"
        local cm_deleted=0
        for cm_uuid in $(echo "$cost_models_response" | jq -r '.data[].uuid // empty'); do
            log_verbose "Deleting cost model: $cm_uuid"
            local cm_delete_response
            cm_delete_response=$(curl -sk -X DELETE "${cost_models_url}${cm_uuid}/" \
                -H "Authorization: Bearer ${access_token}" \
                -H "Content-Type: application/json" \
                -w "%{http_code}" -o /dev/null 2>/dev/null)
            
            if [ "$cm_delete_response" = "204" ] || [ "$cm_delete_response" = "200" ]; then
                ((cm_deleted++))
            fi
        done
        log "✓ Deleted $cm_deleted cost models"
    fi
}

# =============================================================================
# Test Execution
# =============================================================================

run_tests() {
    log "Running IQE tests..."
    
    # Set DYNACONF environment
    export ENV_FOR_DYNACONF="cost_onprem"
    
    # Disable Vault (not accessible from local)
    export DYNACONF_IQE_VAULT_LOADER_ENABLED="false"
    export DYNACONF_IQE_VAULT_OIDC_AUTH="false"
    
    # Build test command as array to avoid shell injection via eval
    # --force-default-user is required because the cost_onprem config's Jinja templates
    # don't evaluate correctly. We bypass this by setting the user explicitly.
    # Note: user name must be lowercase to match the DYNACONF_users__cost_onprem_user__* keys
    local test_cmd=(iqe tests plugin cost_management --force-default-user cost_onprem_user -m "$IQE_MARKER")
    
    if [ -n "$IQE_FILTER" ]; then
        test_cmd+=(-k "$IQE_FILTER")
    fi
    
    test_cmd+=(-vv)
    
    if [ "$DRY_RUN" = true ]; then
        log "[DRY RUN] Would execute:"
        log "  ENV_FOR_DYNACONF=cost_onprem"
        log "  DYNACONF_IQE_VAULT_LOADER_ENABLED=false"
        log "  DYNACONF_IQE_VAULT_OIDC_AUTH=false"
        log "  ${test_cmd[*]}"
        return
    fi
    
    log "Test command: ${test_cmd[*]}"
    log ""
    log "========== Test Output =========="
    
    # Execute tests using array expansion (safe from injection)
    "${test_cmd[@]}"
    local exit_code=$?
    
    log "========== End Test Output =========="
    log ""
    
    if [ $exit_code -eq 0 ]; then
        log "✓ Tests passed"
    else
        log "✗ Tests failed (exit code: $exit_code)"
    fi
    
    return $exit_code
}

# =============================================================================
# Main
# =============================================================================

main() {
    echo "========== IQE Local Test Runner =========="
    echo "Namespace: ${NAMESPACE}"
    echo "Marker: ${IQE_MARKER}"
    if [ -n "${IQE_FILTER}" ]; then
        echo "Filter: ${IQE_FILTER}"
    fi
    echo "IQE Core: ${IQE_CORE_PATH}"
    echo "IQE Plugin: ${IQE_PLUGIN_PATH}"
    echo "Venv: ${VENV_PATH}"
    if [ -n "$NISE_VERSION" ]; then
        echo "NISE Version Override: ${NISE_VERSION}"
    fi
    echo ""
    
    # Validate prerequisites
    validate_prerequisites
    
    # Setup or activate venv
    if [ "$DO_SETUP" = true ]; then
        setup_venv
    else
        activate_venv
    fi
    
    # If only doing setup, exit here
    if [ "$DO_SETUP" = true ] && [ "$DRY_RUN" != true ]; then
        log ""
        log "Setup complete. Run tests with:"
        log "  ./scripts/run-iqe-tests-local.sh"
        log ""
        log "Or with GPU test filter:"
        log "  ./scripts/run-iqe-tests-local.sh --filter \"not ai_workloads and not distro\""
        exit 0
    fi
    
    # Ensure masu is routable (must run before extract_cluster_config)
    ensure_masu_route
    
    # Extract cluster configuration
    extract_cluster_config
    
    # Setup SSL certificates
    setup_ssl_certs
    
    # Clean sources if requested
    clean_sources
    
    # Run tests
    run_tests
}

main "$@"
