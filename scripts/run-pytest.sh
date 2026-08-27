#!/bin/bash

# Run pytest test suite for Cost On-Prem
#
# This script orchestrates the pytest test suite, handling:
# - Virtual environment setup
# - Dependency installation
# - Test execution with JUnit XML reporting
# - Exit code propagation
#
# Usage:
#   ./run-pytest.sh [OPTIONS] [PYTEST_ARGS...]
#
# Suite Options (run specific test suites):
#   --helm              Run Helm chart validation tests
#   --auth              Run JWT authentication tests
#   --infrastructure    Run infrastructure health tests (DB, S3, Kafka)
#   --cost-management   Run Cost Management (Koku) pipeline tests
#   --sources           Run Sources API tests (CRUD, auth, schemas)
#   --ros               Run ROS/Kruize recommendation tests
#   --e2e               Run end-to-end tests
#   --ui                Run UI tests only (Playwright browser automation)
#   --no-ui             Exclude UI, Helm, ROS, and performance tests
#   --performance       Run performance tests (FLPATH-4036)
#   --perf-ingestion    Run ingestion throughput tests only
#   --perf-api          Run API latency tests only
#   --perf-scale        Run scale tests only
#   --perf-ros          Run ROS/Kruize performance tests only
#   --perf-soak         Run soak/stability tests only
#   --perf-valkey       Run Valkey eviction correlation tests only
#   --perf-db           Run PostgreSQL resource sweep tests only
#   --perf-kafka        Run Kafka throughput/scaling tests only
#   --perf-stress          Run stress ramp + recovery tests only
#   --perf-stress-ramp     Run stress ramp-to-failure only (PERF-STR-001)
#   --perf-stress-recovery Run stress backlog recovery only (PERF-STR-002)
#
# Filter Options:
#   --smoke             Run only smoke tests (quick validation)
#   --slow              Include slow tests (processing, recommendations)
#
# Setup Options:
#   --setup-only        Only setup the environment, don't run tests
#   --no-venv           Skip virtual environment (use system Python)
#   --help              Show this help message
#
# Environment Variables:
#   NAMESPACE              Target namespace (default: cost-onprem)
#   HELM_RELEASE_NAME      Helm release name (default: cost-onprem)
#   KEYCLOAK_NAMESPACE     Keycloak namespace (default: keycloak)
#   PYTHON                 Python interpreter (default: python3)
#
# Examples:
#   ./run-pytest.sh                         # Run all tests (including UI)
#   ./run-pytest.sh --no-ui                 # Run all tests except UI, Helm, ROS, and performance
#   ./run-pytest.sh --smoke                 # Run smoke tests only
#   ./run-pytest.sh --helm                  # Run Helm suite only
#   ./run-pytest.sh --auth --ros            # Run auth and ROS suites
#   ./run-pytest.sh --e2e --smoke           # Run E2E smoke tests
#   ./run-pytest.sh --e2e                   # Run full E2E flow
#   ./run-pytest.sh --ui                    # Run UI tests only
#   ./run-pytest.sh --performance           # Run all performance tests
#   ./run-pytest.sh --perf-api              # Run API latency tests only
#   ./run-pytest.sh -k "test_jwt"           # Run tests matching pattern
#   ./run-pytest.sh suites/helm/            # Run specific suite directory
#   ./run-pytest.sh -m "smoke and auth"     # Custom marker expression
#
# Performance Testing (FLPATH-4036):
#   Performance tests are ALWAYS excluded by default to prevent them from
#   running in CI chart test pipelines. Use --performance or --perf-* flags
#   to run them explicitly.
#
#   Environment variables for performance tests:
#     PERF_PROFILE                 Profile to use: baseline, small, medium, large, xlarge
#     PERF_ING_005_DURATION_MINUTES  Duration for high-frequency test (default: 15)
#     CLUSTER_PLATFORM             Platform identifier for reports (e.g., bare-metal, AWS)
#
# Note: UI tests require Playwright and system dependencies. The script will
# automatically install Playwright browsers when UI tests are included.

set -e

# Homebrew/macOS often exports REQUESTS_CA_BUNDLE, which overrides session.verify=False.
unset REQUESTS_CA_BUNDLE SSL_CERT_FILE 2>/dev/null || true

# Script configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TESTS_DIR="${PROJECT_ROOT}/test/pytest"
VENV_DIR="${TESTS_DIR}/.venv"
REPORTS_DIR="${TESTS_DIR}/reports"

# Default configuration
PYTHON="${PYTHON:-python3}"
USE_VENV=true
SETUP_ONLY=false

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

show_help() {
    sed -n '/^# Usage:/,/^set -e$/p' "$0" | grep '^#' | sed 's/^# \?//'
    echo ""
    echo "Available Test Suites:"
    echo "  helm              Helm chart lint, template, deployment health"
    echo "  auth              Keycloak, JWT ingress/backend authentication"
    echo "  infrastructure    Database, S3, Kafka health checks"
    echo "  cost-management   Upload, Koku processing pipeline"
    echo "  sources           Sources API CRUD, authentication, schemas"
    echo "  ros               Kruize, recommendations API"
    echo "  e2e               Complete end-to-end data flow"
    echo "  ui                Browser-based UI tests (Playwright)"
    echo "  performance       Performance tests (ingestion, API, scale)"
    echo ""
    echo "Markers:"
    echo "  smoke             Quick validation tests (~1 min)"
    echo "  slow              Long-running tests (processing, recommendations)"
    echo ""
    echo "Performance Test Options:"
    echo "  --performance     Run all performance tests"
    echo "  --perf-ingestion  Run ingestion throughput tests (PERF-ING-*)"
    echo "  --perf-api        Run API latency tests (PERF-API-*)"
    echo "  --perf-scale      Run scale tests (PERF-SCALE-*)"
    echo "  --perf-ros        Run ROS/Kruize performance tests (PERF-ROS-*)"
    echo "  --perf-soak       Run soak/stability tests (PERF-SOAK-*)"
    echo "  --perf-valkey     Run Valkey eviction correlation tests (PERF-VK-*)"
    echo "  --perf-db         Run PostgreSQL resource sweep tests (PERF-DB-*)"
    echo "  --perf-kafka      Run Kafka throughput/scaling tests (PERF-KAF-*)"
    echo "  --perf-celery     Run Celery worker scaling tests (PERF-CEL-*)"
    echo "  --perf-stress          Run stress ramp + recovery tests (PERF-STR-*)"
    echo "  --perf-stress-ramp     Run stress ramp-to-failure only (PERF-STR-001)"
    echo "  --perf-stress-recovery Run stress backlog recovery only (PERF-STR-002)"
    echo ""
    echo "UI Tests:"
    echo "  UI tests are included by default. Use --no-ui to exclude UI, Helm, ROS, and performance."
    echo "  Use --ui to run ONLY UI tests."
    exit 0
}

check_prerequisites() {
    log_info "Checking prerequisites..."

    # Check Python
    if ! command -v "$PYTHON" &> /dev/null; then
        log_error "Python not found: $PYTHON"
        log_error "Please install Python 3.10+ or set PYTHON environment variable"
        exit 1
    fi

    local python_version
    python_version=$("$PYTHON" -c "import sys; print(f'{sys.version_info.major}.{sys.version_info.minor}')")
    log_info "Using Python $python_version"

    # Check if we're logged into OpenShift
    if ! command -v oc &> /dev/null; then
        log_error "oc CLI not found. Please install OpenShift CLI."
        exit 1
    fi

    if ! oc whoami &> /dev/null; then
        log_error "Not logged into OpenShift. Please run 'oc login' first."
        exit 1
    fi

    log_success "Prerequisites check passed"
}

setup_venv() {
    if [[ "$USE_VENV" != "true" ]]; then
        log_info "Skipping virtual environment setup (--no-venv)"
        return 0
    fi

    log_info "Setting up virtual environment..."

    if [[ ! -d "$VENV_DIR" ]]; then
        log_info "Creating virtual environment at $VENV_DIR"
        "$PYTHON" -m venv "$VENV_DIR"
    fi

    # Activate virtual environment
    # shellcheck source=/dev/null
    source "$VENV_DIR/bin/activate"

    # Upgrade pip
    pip install --quiet --upgrade pip

    # Install dependencies
    log_info "Installing test dependencies..."
    pip install --quiet -r "$TESTS_DIR/requirements.txt"

    log_success "Virtual environment ready"
}

install_playwright_browsers() {
    # Install Playwright browsers (required for UI tests)
    # Note: Playwright requires system libraries (libnspr4, libnss3, etc.)
    # In CI, these must be installed by the CI step before running tests.
    # See: https://playwright.dev/python/docs/browsers#install-system-dependencies
    if ! command -v playwright &> /dev/null; then
        log_error "Playwright not found in PATH"
        return 1
    fi
    
    log_info "Installing Playwright browsers for UI tests..."
    # Try with system deps first (requires root), fall back to browser-only install
    if playwright install chromium --with-deps 2>/dev/null; then
        log_success "Playwright browsers installed with system dependencies"
    elif playwright install chromium 2>/dev/null; then
        log_warning "Playwright browsers installed WITHOUT system deps - may fail at runtime"
        log_warning "Linux requires: dnf install -y nspr nss nss-util atk cups-libs libdrm libXcomposite libXdamage libXrandr mesa-libgbm pango alsa-lib"
    else
        log_error "Failed to install Playwright browsers"
        return 1
    fi
}

setup_reports_dir() {
    log_info "Setting up reports directory..."
    mkdir -p "$REPORTS_DIR"
    log_success "Reports will be written to: $REPORTS_DIR"
}

run_pytest() {
    local pytest_args=("$@")

    log_info "Running pytest..."
    log_info "  Namespace: ${NAMESPACE:-cost-onprem}"
    log_info "  Helm Release: ${HELM_RELEASE_NAME:-cost-onprem}"
    log_info "  Keycloak Namespace: ${KEYCLOAK_NAMESPACE:-keycloak}"
    echo ""

    # Export environment variables for tests
    export NAMESPACE="${NAMESPACE:-cost-onprem}"
    export HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
    export KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"

    if command -v kubectl >/dev/null 2>&1; then
        log_info "Waiting for pods ready (instance=${HELM_RELEASE_NAME})..."
        if ! kubectl wait --for=condition=ready pod \
            -l "app.kubernetes.io/instance=${HELM_RELEASE_NAME}" \
            -n "${NAMESPACE}" \
            --timeout=300s \
            --field-selector=status.phase!=Succeeded; then
            log_warning "Pod ready wait timed out; continuing (fixtures may fail if pods still initializing)"
        fi
    fi

    # Change to tests directory
    cd "$TESTS_DIR"

    # Check if pytest-html is available and add HTML reporting if so
    local html_args=()
    local html_report_path="reports/report.html"
    
    # For performance tests with unified output structure, use PERF_REPORTS_DIR
    if [[ -n "${PERF_REPORTS_DIR:-}" ]]; then
        html_report_path="${PERF_REPORTS_DIR}/report.html"
    fi
    
    if python -c "import pytest_html" 2>/dev/null; then
        log_info "pytest-html available, enabling HTML report"
        html_args=("--html=${html_report_path}" "--self-contained-html")
    else
        log_warning "pytest-html not available, skipping HTML report"
    fi

    # Log the full pytest command being executed (critical for CI debugging)
    echo ""
    echo "============================================================"
    echo "PYTEST COMMAND"
    echo "============================================================"
    echo "pytest ${html_args[*]} ${pytest_args[*]}"
    echo ""
    echo "Working directory: $(pwd)"
    echo "NAMESPACE=${NAMESPACE}"
    echo "HELM_RELEASE_NAME=${HELM_RELEASE_NAME}"
    echo "KEYCLOAK_NAMESPACE=${KEYCLOAK_NAMESPACE}"
    echo "============================================================"
    echo ""

    # Run pytest with JUnit XML output (HTML report added if available)
    local exit_code=0
    pytest "${html_args[@]}" "${pytest_args[@]}" || exit_code=$?

    return $exit_code
}

main() {
    local pytest_markers=()
    local pytest_extra_args=()
    local include_ui=true   # UI tests included by default
    local exclude_ui=false  # Flag to explicitly exclude UI
    local ui_only=false     # Flag for running only UI tests

    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            # Suite options
            --helm)
                pytest_markers+=("helm")
                shift
                ;;
            --auth)
                pytest_markers+=("auth")
                shift
                ;;
            --infrastructure)
                pytest_markers+=("infrastructure")
                shift
                ;;
            --cost-management)
                pytest_markers+=("cost_management")
                shift
                ;;
            --sources)
                pytest_markers+=("sources")
                shift
                ;;
            --ros)
                pytest_markers+=("ros")
                shift
                ;;
            --e2e)
                pytest_markers+=("e2e")
                shift
                ;;
            --ui)
                # Run ONLY UI tests
                ui_only=true
                shift
                ;;
            --no-ui)
                # Exclude UI tests
                exclude_ui=true
                include_ui=false
                shift
                ;;
            --performance)
                # Run all performance tests (no UI needed)
                pytest_markers+=("performance")
                include_ui=false
                shift
                ;;
            --perf-ingestion)
                # Run ingestion throughput tests only (no UI needed)
                pytest_markers+=("performance and ingestion")
                include_ui=false
                shift
                ;;
            --perf-api)
                # Run API latency tests only (no UI needed)
                pytest_markers+=("performance and api_latency")
                include_ui=false
                shift
                ;;
            --perf-scale)
                # Run scale tests only (no UI needed)
                pytest_markers+=("performance and scale")
                include_ui=false
                shift
                ;;
            --perf-ros)
                # Run ROS/Kruize performance tests only (no UI needed)
                pytest_markers+=("performance and ros_perf")
                include_ui=false
                shift
                ;;
            --perf-soak)
                # Run soak/stability tests only (no UI needed)
                pytest_markers+=("performance and soak")
                include_ui=false
                shift
                ;;
            --perf-valkey)
                # Run Valkey eviction correlation tests (COST-7605 DB-3)
                pytest_markers+=("performance and valkey_eviction")
                include_ui=false
                shift
                ;;
            --perf-db)
                # Run PostgreSQL resource sweep tests (COST-7605 DB-1, DB-2)
                pytest_markers+=("performance and db_sweep")
                include_ui=false
                shift
                ;;
            --perf-kafka)
                # Run Kafka throughput/scaling tests (COST-7638)
                pytest_markers+=("performance and kafka_throughput")
                include_ui=false
                shift
                ;;
            --perf-celery)
                # Run Celery worker scaling tests (COST-7598)
                pytest_markers+=("performance and celery_scaling")
                include_ui=false
                shift
                ;;
            --perf-stress)
                # Run stress ramp + recovery tests (COST-7627)
                pytest_markers+=("performance and stress")
                include_ui=false
                shift
                ;;
            --perf-stress-ramp)
                pytest_markers+=("performance and stress_ramp")
                include_ui=false
                shift
                ;;
            --perf-stress-recovery)
                pytest_markers+=("performance and stress_recovery")
                include_ui=false
                shift
                ;;
            # Filter options
            --smoke)
                pytest_markers+=("smoke")
                shift
                ;;
            --slow)
                pytest_markers+=("slow")
                shift
                ;;
            # Setup options
            --setup-only)
                SETUP_ONLY=true
                shift
                ;;
            --no-venv)
                USE_VENV=false
                shift
                ;;
            --help|-h)
                show_help
                ;;
            -m)
                # Check if marker expression explicitly excludes UI
                if [[ "$2" == *"not ui"* ]]; then
                    include_ui=false
                    exclude_ui=true
                elif [[ "$2" == "ui" ]]; then
                    ui_only=true
                fi
                pytest_extra_args+=("$1" "$2")
                shift 2
                ;;
            *)
                # Pass through to pytest
                pytest_extra_args+=("$1")
                shift
                ;;
        esac
    done

    echo ""
    echo -e "${BLUE}Cost On-Prem Test Suite${NC}"
    echo "========================"
    echo ""

    # Check prerequisites
    check_prerequisites

    # Setup virtual environment
    setup_venv

    # Install Playwright browsers if UI tests will be included
    if [[ "$include_ui" == "true" ]] || [[ "$ui_only" == "true" ]]; then
        install_playwright_browsers
    fi

    # Setup reports directory
    setup_reports_dir

    if [[ "$SETUP_ONLY" == "true" ]]; then
        log_success "Environment setup complete"
        exit 0
    fi

    # Build pytest arguments
    local pytest_args=()
    local is_perf_test=false
    local perf_reports_dir=""

    # Handle marker filtering
    # Performance tests are ALWAYS excluded unless explicitly requested via --performance flags
    # This prevents long-running perf tests from running during normal chart test CI
    if [[ "$ui_only" == "true" ]]; then
        # Run only UI tests
        pytest_args+=("-m" "ui")
    elif [[ ${#pytest_markers[@]} -gt 0 ]]; then
        local marker_expr=""
        for _m in "${pytest_markers[@]}"; do
            if [[ -n "$marker_expr" ]]; then
                marker_expr="($marker_expr) or ($_m)"
            else
                marker_expr="$_m"
            fi
        done
        # Check if this is a performance test request
        if [[ "$marker_expr" == *"performance"* ]]; then
            # Performance tests - use marker as-is
            pytest_args+=("-m" "$marker_expr")
            is_perf_test=true
        else
            # Non-performance tests - exclude performance marker
            pytest_args+=("-m" "($marker_expr) and not performance")
        fi
    elif [[ "$exclude_ui" == "true" ]]; then
        # Exclude UI, Helm, ROS (beta), and performance when --no-ui is specified
        pytest_args+=("-m" "not ui and not performance and not helm and not ros")
    else
        # Default: run all tests EXCEPT performance tests
        # Performance tests must be explicitly requested via --performance flags
        pytest_args+=("-m" "not performance and not helm")
    fi

    # For performance tests with unified output structure, redirect reports to PERF_OUTPUT_DIR
    if [[ "$is_perf_test" == "true" ]] && [[ -n "${PERF_OUTPUT_DIR:-}" ]] && [[ -n "${TEST_RUN_ID:-}" ]]; then
        # PERF_OUTPUT_DIR should be an absolute path (set by deploy-test-cost-onprem.sh)
        # If it's relative, make it absolute relative to PROJECT_ROOT
        local perf_output_dir_resolved="${PERF_OUTPUT_DIR}"
        if [[ "$perf_output_dir_resolved" != /* ]]; then
            perf_output_dir_resolved="${PROJECT_ROOT}/${PERF_OUTPUT_DIR}"
        fi
        
        perf_reports_dir="${perf_output_dir_resolved}/${TEST_RUN_ID}/reports"
        mkdir -p "$perf_reports_dir"
        # Override default junit-xml path from pytest.ini
        pytest_args+=("--junit-xml=${perf_reports_dir}/junit.xml")
        # Export for run_pytest to use for HTML report
        export PERF_REPORTS_DIR="$perf_reports_dir"
        log_info "Performance test reports will be saved to: ${perf_reports_dir}/"
    fi

    # Add any extra arguments
    if [[ ${#pytest_extra_args[@]} -gt 0 ]]; then
        pytest_args+=("${pytest_extra_args[@]}")
    fi

    # Run tests
    local exit_code=0
    run_pytest "${pytest_args[@]}" || exit_code=$?

    echo ""
    if [[ $exit_code -eq 0 ]]; then
        log_success "All tests passed!"
    else
        log_error "Some tests failed (exit code: $exit_code)"
    fi

    # Show appropriate reports location
    if [[ -n "$perf_reports_dir" ]]; then
        log_info "JUnit report: ${perf_reports_dir}/junit.xml"
    else
        log_info "JUnit report: $REPORTS_DIR/junit.xml"
    fi

    exit $exit_code
}

main "$@"
