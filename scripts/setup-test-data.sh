#!/usr/bin/env bash
#
# Setup test data for Cost On-Prem testing.
#
# This script provides scenario-based data setup for different test types,
# including E2E validation, performance testing, and ROS testing.
#
# Usage:
#   ./scripts/setup-test-data.sh --scenario <scenario> [options]
#
# Examples:
#   ./scripts/setup-test-data.sh --list                    # List scenarios
#   ./scripts/setup-test-data.sh --scenario minimal        # Quick validation
#   ./scripts/setup-test-data.sh --scenario baseline       # Standard E2E
#   ./scripts/setup-test-data.sh --scenario perf-small     # Performance testing
#   ./scripts/setup-test-data.sh --clean                   # Clean test data
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TESTS_DIR="${REPO_ROOT}/tests"

# Track whether setup completed so the trap knows if cleanup advice is needed
_SETUP_COMPLETED=false

_on_exit() {
    local exit_code=$?
    if [[ "$exit_code" -ne 0 && "$_SETUP_COMPLETED" != "true" && "$CLEAN_ONLY" != "true" ]]; then
        echo ""
        echo -e "${RED}[setup-test-data] Script interrupted (exit $exit_code).${NC}"
        echo -e "${YELLOW}Sources may have been created. To clean up:${NC}"
        echo "  ./scripts/setup-test-data.sh --clean --clean-prefix ${SOURCE_PREFIX}-${SCENARIO:-unknown}"
        echo ""
    fi
}
trap _on_exit EXIT

# Defaults
NAMESPACE="${NAMESPACE:-cost-onprem}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-cost-onprem}"
KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
KAFKA_NAMESPACE="${KAFKA_NAMESPACE:-kafka}"

# Scenario defaults
SCENARIO=""
DAYS=7
CLUSTERS=1
SOURCE_PREFIX="e2e"
WAIT_FOR_PROCESSING=true
DRY_RUN=false
CLEAN_ONLY=false
NO_CLEANUP=false

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log() { echo -e "${BLUE}[setup-test-data]${NC} $*"; }
log_success() { echo -e "${GREEN}[setup-test-data]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[setup-test-data]${NC} $*"; }
log_error() { echo -e "${RED}[setup-test-data]${NC} $*" >&2; }

# Retry a command with exponential backoff
# Usage: retry <max_attempts> <delay_seconds> <command...>
retry() {
    local max_attempts=$1
    local delay=$2
    shift 2
    local attempt=1
    
    while [[ $attempt -le $max_attempts ]]; do
        if "$@"; then
            return 0
        fi
        
        if [[ $attempt -lt $max_attempts ]]; then
            log_warn "Command failed (attempt $attempt/$max_attempts), retrying in ${delay}s..."
            sleep "$delay"
            delay=$((delay * 2))  # Exponential backoff
        fi
        attempt=$((attempt + 1))
    done
    
    log_error "Command failed after $max_attempts attempts: $*"
    return 1
}

# Run oc command with retry for transient failures
run_oc() {
    retry 3 2 oc "$@"
}

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Setup test data for Cost On-Prem testing.

OPTIONS:
    --scenario <name>     Scenario to set up (see --list for options)
    --list                List available scenarios
    --days <n>            Number of days of data (default: 7)
    --clusters <n>        Number of clusters (default: 1)
    --source-prefix <s>   Prefix for source names (default: e2e)
    --no-wait             Don't wait for processing to complete
    --no-cleanup          Keep data after script exits (for manual exploration)
    --dry-run             Show what would be done without doing it
    --clean               Clean test data only (no setup)
    --clean-prefix <s>    Clean sources matching prefix (use with --clean)
    --help                Show this help message

SCENARIOS:
    minimal       Single pod, single namespace (~30s upload, ~2min processing)
    baseline      Standard E2E setup (~2min upload, ~10min processing)
    perf-small    Performance small profile (~5min upload, ~30min processing)
    perf-medium   Performance medium profile (~15min upload, ~60min processing)
    perf-large    Performance large profile (~45min upload, ~3hr processing)
    ros           ROS recommendation testing (~2min upload, ~15min processing)

ENVIRONMENT:
    NAMESPACE           Target namespace (default: cost-onprem)
    HELM_RELEASE_NAME   Helm release name (default: cost-onprem)
    KEYCLOAK_NAMESPACE  Keycloak namespace (default: keycloak)
    KAFKA_NAMESPACE     Kafka namespace (default: kafka)

EXAMPLES:
    # List available scenarios
    $(basename "$0") --list

    # Quick validation
    $(basename "$0") --scenario minimal

    # Standard E2E with 7 days of data
    $(basename "$0") --scenario baseline --days 7

    # Performance testing with multiple clusters
    $(basename "$0") --scenario perf-medium --clusters 3

    # Setup data for manual exploration (won't auto-cleanup)
    $(basename "$0") --scenario baseline --no-cleanup --source-prefix demo

    # Clean all test data
    $(basename "$0") --clean

    # Clean specific sources
    $(basename "$0") --clean --clean-prefix demo-
EOF
}

list_scenarios() {
    cat <<EOF
Available scenarios:

  SCENARIO      CLUSTERS  NODES  DAYS  ROS   UPLOAD    PROCESSING  USE CASE
  ──────────────────────────────────────────────────────────────────────────
  minimal       1         1      1     No    <30s      <2min       Smoke tests
  baseline      1         2      7     Yes   <2min     <10min      E2E tests
  perf-small    1         15     30    Yes   <5min     <30min      Perf baseline
  perf-medium   2         49     30    Yes   <15min    <60min      Scale testing
  perf-large    7         133    30    Yes   <45min    <3hr        Stress testing
  ros           1         3      7     Yes   <2min     <15min      ROS testing

Notes:
  - All scenarios generate valid OCP cost data
  - ROS scenarios include resource optimization data for Kruize
  - Processing times are estimates based on typical deployments
  - Use --days to override the default data duration

EOF
}

# Scenario configurations (nodes, pods_per_node, namespaces, ros_enabled)
get_scenario_config() {
    local scenario=$1
    case "$scenario" in
        minimal)
            echo "1 1 1 false"
            ;;
        baseline)
            echo "2 10 3 true"
            ;;
        perf-small)
            echo "15 13 10 true"
            ;;
        perf-medium)
            echo "49 10 20 true"
            ;;
        perf-large)
            echo "133 15 30 true"
            ;;
        ros)
            echo "3 10 5 true"
            ;;
        *)
            log_error "Unknown scenario: $scenario"
            exit 1
            ;;
    esac
}

# Check prerequisites
check_prerequisites() {
    log "Checking prerequisites..."

    # Check oc CLI
    if ! command -v oc &> /dev/null; then
        log_error "oc CLI not found. Please install OpenShift CLI."
        exit 1
    fi

    # Skip cluster checks in dry-run mode
    if [[ "$DRY_RUN" == "true" ]]; then
        log "Skipping cluster checks (dry-run mode)"
        return 0
    fi

    # Check cluster access
    if ! oc whoami &> /dev/null; then
        log_error "Not logged into OpenShift cluster. Run 'oc login' first."
        exit 1
    fi

    # Check namespace exists
    if ! oc get namespace "$NAMESPACE" &> /dev/null; then
        log_error "Namespace '$NAMESPACE' not found."
        exit 1
    fi

    # Check deployment exists
    if ! oc get deployment -n "$NAMESPACE" "${HELM_RELEASE_NAME}-koku-api" &> /dev/null; then
        log_error "Cost On-Prem deployment not found in namespace '$NAMESPACE'."
        log_error "Deploy first with: ./scripts/deploy-test-cost-onprem.sh"
        exit 1
    fi
    
    # Check Python venv
    if [[ ! -d "${TESTS_DIR}/.venv" ]]; then
        log "Creating Python virtual environment..."
        python3 -m venv "${TESTS_DIR}/.venv"
    fi
    
    # Activate venv
    # shellcheck source=/dev/null
    source "${TESTS_DIR}/.venv/bin/activate"
    
    # Install dependencies if needed
    if ! python3 -c "import requests" &> /dev/null; then
        log "Installing Python dependencies..."
        pip install -q -r "${TESTS_DIR}/requirements.txt"
    fi
    
    # Check/install NISE
    if ! python3 -c "import nise" &> /dev/null; then
        log "Installing koku-nise..."
        pip install -q koku-nise
    fi
    
    log_success "Prerequisites OK"
}

# Clean test data
clean_test_data() {
    local prefix="${1:-e2e-pytest-}"
    
    log "Cleaning test data with prefix: $prefix"
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log "[DRY RUN] Would clean sources matching: ${prefix}*"
        return 0
    fi
    
    # Get DB pod with retry
    local db_pod
    db_pod=$(run_oc get pods -n "$NAMESPACE" -l app.kubernetes.io/component=database -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    
    if [[ -z "$db_pod" ]]; then
        log_warn "Database pod not found, skipping DB cleanup"
        return 0
    fi
    
    # Delete sources via API
    log "Deleting sources matching prefix '$prefix'..."
    
    # Get source IDs with retry
    local sources
    sources=$(run_oc exec -n "$NAMESPACE" "$db_pod" -- \
        psql -h localhost -U koku -d koku -t -c \
        "SELECT id, name FROM api_sources WHERE name LIKE '${prefix}%';" 2>/dev/null || true)
    
    if [[ -z "$sources" ]]; then
        log "No sources found matching prefix '$prefix'"
    else
        echo "$sources" | while read -r line; do
            if [[ -n "$line" ]]; then
                local id name
                id=$(echo "$line" | awk -F'|' '{print $1}' | tr -d ' ')
                name=$(echo "$line" | awk -F'|' '{print $2}' | tr -d ' ')
                if [[ -n "$id" && -n "$name" ]]; then
                    log "  Deleting source $id ($name)..."
                    run_oc exec -n "$NAMESPACE" "$db_pod" -- \
                        psql -h localhost -U koku -d koku -c \
                        "DELETE FROM api_sources WHERE id = $id;" &>/dev/null || true
                fi
            fi
        done
    fi
    
    # Clean orphaned data
    log "Cleaning orphaned manifests and reports..."
    run_oc exec -n "$NAMESPACE" "$db_pod" -- \
        psql -h localhost -U koku -d koku -c \
        "DELETE FROM reporting_ocpusagereportmanifest WHERE cluster_id LIKE '${prefix}%';" &>/dev/null || true
    
    # Flush cache
    log "Flushing cache..."
    local valkey_pod
    valkey_pod=$(run_oc get pods -n "$NAMESPACE" -l app.kubernetes.io/component=valkey -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -n "$valkey_pod" ]]; then
        run_oc exec -n "$NAMESPACE" "$valkey_pod" -- valkey-cli FLUSHALL &>/dev/null || true
    fi
    
    log_success "Cleanup complete"
}

# Setup test data using Python
setup_test_data() {
    local scenario=$1
    
    log "Setting up test data for scenario: $scenario"
    
    # Get scenario config
    local config
    config=$(get_scenario_config "$scenario")
    read -r nodes pods_per_node namespaces ros_enabled <<< "$config"
    
    log "  Clusters: $CLUSTERS"
    log "  Nodes per cluster: $nodes"
    log "  Pods per node: $pods_per_node"
    log "  Namespaces: $namespaces"
    log "  Days of data: $DAYS"
    log "  ROS enabled: $ros_enabled"
    log "  Source prefix: $SOURCE_PREFIX"
    
    if [[ "$DRY_RUN" == "true" ]]; then
        log "[DRY RUN] Would generate and upload data with above configuration"
        return 0
    fi
    
    # Create Python script for data setup
    local setup_script
    setup_script=$(mktemp)
    
    cat > "$setup_script" <<'PYTHON_SCRIPT'
#!/usr/bin/env python3
"""Setup test data for Cost On-Prem."""

import os
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

# Add tests directory to path
sys.path.insert(0, os.environ.get('TESTS_DIR', '.'))

from conftest import obtain_jwt_token, ClusterConfig
from e2e_helpers import (
    generate_cluster_id,
    register_source,
    wait_for_provider,
)
from suites.performance.test_ingestion import generate_and_upload_data

def main():
    # Get configuration from environment
    namespace = os.environ.get('NAMESPACE', 'cost-onprem')
    helm_release = os.environ.get('HELM_RELEASE_NAME', 'cost-onprem')
    keycloak_ns = os.environ.get('KEYCLOAK_NAMESPACE', 'keycloak')
    
    scenario = os.environ.get('SCENARIO', 'baseline')
    days = int(os.environ.get('DAYS', '7'))
    clusters = int(os.environ.get('CLUSTERS', '1'))
    source_prefix = os.environ.get('SOURCE_PREFIX', 'e2e')
    ros_enabled = os.environ.get('ROS_ENABLED', 'true').lower() == 'true'
    wait_processing = os.environ.get('WAIT_PROCESSING', 'true').lower() == 'true'
    
    print(f"Setting up test data...")
    print(f"  Scenario: {scenario}")
    print(f"  Days: {days}")
    print(f"  Clusters: {clusters}")
    print(f"  Source prefix: {source_prefix}")
    print(f"  ROS enabled: {ros_enabled}")
    
    # Create cluster config
    cluster_config = ClusterConfig(
        namespace=namespace,
        helm_release_name=helm_release,
        keycloak_namespace=keycloak_ns,
        platform="openshift",
    )
    
    # Get JWT token
    print("Obtaining JWT token...")
    jwt_token = obtain_jwt_token(cluster_config)
    
    # Get gateway URL
    import subprocess
    result = subprocess.run(
        ['oc', 'get', 'route', '-n', namespace, f'{helm_release}-gateway', 
         '-o', 'jsonpath={.spec.host}'],
        capture_output=True, text=True
    )
    gateway_host = result.stdout.strip()
    if not gateway_host:
        print("ERROR: Could not get gateway route")
        sys.exit(1)
    
    gateway_url = f"https://{gateway_host}"
    ingress_url = f"{gateway_url}/api/ingress"
    
    print(f"Gateway URL: {gateway_url}")
    
    # Set up each cluster
    created_sources = []
    
    for i in range(clusters):
        cluster_id = generate_cluster_id()
        source_name = f"{source_prefix}-{scenario}-{i+1:02d}-{cluster_id[:8]}"
        
        print(f"\nSetting up cluster {i+1}/{clusters}:")
        print(f"  Cluster ID: {cluster_id}")
        print(f"  Source name: {source_name}")
        
        # Register source
        # We need to get ingress pod and koku API URL for registration
        result = subprocess.run(
            ['oc', 'get', 'pods', '-n', namespace, '-l', 
             'app.kubernetes.io/component=koku-ingress', '-o', 
             'jsonpath={.items[0].metadata.name}'],
            capture_output=True, text=True
        )
        ingress_pod = result.stdout.strip()
        
        koku_api_url = f"{helm_release}-koku-api.{namespace}.svc.cluster.local:8000"
        
        print(f"  Registering source...")
        source = register_source(
            namespace,
            ingress_pod,
            koku_api_url,
            jwt_token.identity_header,
            cluster_id,
            "1234567",
            source_name,
        )
        
        created_sources.append({
            'source_id': source.source_id,
            'source_name': source_name,
            'cluster_id': cluster_id,
        })
        
        # Generate and upload data
        end_date = datetime.now(timezone.utc)
        start_date = end_date - timedelta(days=days)
        
        print(f"  Generating and uploading data ({days} days)...")
        
        # Map scenario to profile
        profile_map = {
            'minimal': 'baseline',
            'baseline': 'baseline', 
            'perf-small': 'small',
            'perf-medium': 'medium',
            'perf-large': 'large',
            'ros': 'baseline',
        }
        profile = profile_map.get(scenario, 'baseline')
        
        result = generate_and_upload_data(
            cluster_id=cluster_id,
            source_name=source_name,
            start_date=start_date,
            end_date=end_date,
            ingress_url=ingress_url,
            jwt_token=jwt_token,
            profile_name=profile,
        )
        
        if result.get('upload_status') != 202:
            print(f"  ERROR: Upload failed with status {result.get('upload_status')}")
            continue
            
        print(f"  Upload successful:")
        print(f"    Package size: {result.get('package_size_mb', 0):.2f} MB")
        print(f"    Upload time: {result.get('upload_seconds', 0):.1f}s")
        print(f"    Generation time: {result.get('generation_seconds', 0):.1f}s")
    
    # Wait for processing if requested
    if wait_processing and created_sources:
        print("\nWaiting for data processing...")
        
        # Get DB pod
        result = subprocess.run(
            ['oc', 'get', 'pods', '-n', namespace, '-l',
             'app.kubernetes.io/component=database', '-o',
             'jsonpath={.items[0].metadata.name}'],
            capture_output=True, text=True
        )
        db_pod = result.stdout.strip()
        
        for source_info in created_sources:
            print(f"  Waiting for provider: {source_info['source_name']}...")
            success = wait_for_provider(
                namespace,
                db_pod,
                source_info['cluster_id'],
                timeout=600,  # 10 minutes
            )
            if success:
                print(f"    Provider ready")
            else:
                print(f"    WARNING: Provider not ready within timeout")
    
    # Print summary
    print("\n" + "=" * 60)
    print("SETUP COMPLETE")
    print("=" * 60)
    print("\nCreated sources:")
    for source_info in created_sources:
        print(f"  - {source_info['source_name']}")
        print(f"    Cluster ID: {source_info['cluster_id']}")
        print(f"    Source ID: {source_info['source_id']}")
    
    print("\nTo clean up:")
    print(f"  ./scripts/setup-test-data.sh --clean --clean-prefix {source_prefix}-{scenario}")
    
    return 0

if __name__ == '__main__':
    sys.exit(main())
PYTHON_SCRIPT
    
    # Run the setup script
    TESTS_DIR="$TESTS_DIR" \
    NAMESPACE="$NAMESPACE" \
    HELM_RELEASE_NAME="$HELM_RELEASE_NAME" \
    KEYCLOAK_NAMESPACE="$KEYCLOAK_NAMESPACE" \
    SCENARIO="$scenario" \
    DAYS="$DAYS" \
    CLUSTERS="$CLUSTERS" \
    SOURCE_PREFIX="$SOURCE_PREFIX" \
    ROS_ENABLED="$ros_enabled" \
    WAIT_PROCESSING="$WAIT_FOR_PROCESSING" \
    python3 "$setup_script"
    
    local exit_code=$?
    rm -f "$setup_script"
    
    return $exit_code
}

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --scenario)
            SCENARIO="$2"
            shift 2
            ;;
        --list)
            list_scenarios
            exit 0
            ;;
        --days)
            DAYS="$2"
            shift 2
            ;;
        --clusters)
            CLUSTERS="$2"
            shift 2
            ;;
        --source-prefix)
            SOURCE_PREFIX="$2"
            shift 2
            ;;
        --no-wait)
            WAIT_FOR_PROCESSING=false
            shift
            ;;
        --no-cleanup)
            NO_CLEANUP=true
            shift
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --clean)
            CLEAN_ONLY=true
            shift
            ;;
        --clean-prefix)
            SOURCE_PREFIX="$2"
            shift 2
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            usage
            exit 1
            ;;
    esac
done

# Main execution
main() {
    if [[ "$CLEAN_ONLY" == "true" ]]; then
        check_prerequisites
        clean_test_data "$SOURCE_PREFIX"
        exit 0
    fi
    
    if [[ -z "$SCENARIO" ]]; then
        log_error "No scenario specified. Use --scenario <name> or --list to see options."
        usage
        exit 1
    fi
    
    check_prerequisites
    
    # Clean existing data first (unless --no-cleanup)
    if [[ "$NO_CLEANUP" != "true" ]]; then
        clean_test_data "${SOURCE_PREFIX}-${SCENARIO}"
    fi
    
    # Setup new data
    setup_test_data "$SCENARIO"
    
    _SETUP_COMPLETED=true
    log_success "Test data setup complete!"
}

main
