#!/bin/bash

# Cost Management On Premise Helm Chart Installation Script
# This script deploys the Cost Management On Premise Helm chart to an OpenShift cluster
# By default, it installs from the Helm chart repository (GitHub Pages)
# Set USE_LOCAL_CHART=true to use local chart source instead
# Requires: kubectl/oc configured with target cluster context, helm installed, jq
#
# Environment Variables:
#   LOG_LEVEL       - Control output verbosity (ERROR|WARN|INFO|DEBUG, default: WARN)
#   USE_LOCAL_CHART - Use local chart instead of Helm repository (true|false, default: false)
#   CHART_VERSION   - Pin a specific chart version from the Helm repository (default: latest)
#   NAMESPACE       - Target namespace (default: cost-onprem)
#   S3_ENDPOINT     - S3 endpoint hostname for generic S3 backends (e.g., "s3.example.com")
#   S3_PORT         - S3 port (default: 443, used with S3_ENDPOINT)
#   S3_USE_SSL      - Whether S3 uses TLS (default: true, used with S3_ENDPOINT)
#   S3_VERIFY_SSL   - Verify S3 TLS certificates (default: false; internal services use cluster CA)
#   S3_ACCESS_KEY   - S3 access key (creates storage secret when set with S3_SECRET_KEY; also for bucket job)
#   S3_SECRET_KEY   - S3 secret key (creates storage secret when set with S3_ACCESS_KEY)
#   S3_REGION       - S3 region for SigV4 (required for AWS S3; sets objectStorage.s3.region and CLI signing)
#   S3_BUCKET_PREFIX - AWS/public S3: prefix for globally unique buckets (<prefix>-ingress-upload, -koku, -ros)
#   S3_BUCKET_INGRESS / S3_BUCKET_KOKU / S3_BUCKET_ROS - Override individual bucket names (override prefix)
#   SKIP_S3_SETUP   - Skip S3 bucket creation entirely (default: false)
#   S3_CLI_IMAGE    - Container image with AWS CLI for bucket creation (default: amazon/aws-cli:latest)
#   HELM_FORCE_CONFLICTS - When true, pass --force-conflicts to helm upgrade (SSA field ownership;
#                          use after kubectl set / oc set image on chart-managed resources)
#
# Examples:
#   # Default (clean output with successes/warnings/errors only)
#   ./install-helm-chart.sh
#
#   # Detailed output with all info messages
#   LOG_LEVEL=INFO ./install-helm-chart.sh
#
#   # Quiet (errors only)
#   LOG_LEVEL=ERROR ./install-helm-chart.sh
#
#   # Generic S3 backend (non-ODF)
#   S3_ENDPOINT=s3.openshift-storage.svc S3_PORT=443 ./install-helm-chart.sh
#
#   # AWS S3 (credentials via env; use public CA / verify SSL; buckets must be globally unique)
#   S3_ENDPOINT=s3.us-east-1.amazonaws.com S3_REGION=us-east-1 S3_VERIFY_SSL=true \\
#     S3_BUCKET_PREFIX=myorg-costonprem-prod \\
#     S3_ACCESS_KEY=... S3_SECRET_KEY=... ./install-helm-chart.sh

set -e  # Exit on any error

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging configuration
# LOG_LEVEL controls output verbosity:
#   ERROR - Only show errors (quietest)
#   WARN  - Show errors, warnings, and successes (default, clean output)
#   INFO  - Show errors, warnings, successes, and info messages (detailed)
#   DEBUG - Show everything (most verbose)
LOG_LEVEL=${LOG_LEVEL:-WARN}

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELM_RELEASE_NAME=${HELM_RELEASE_NAME:-cost-onprem}
NAMESPACE=${NAMESPACE:-cost-onprem}
VALUES_FILE=${VALUES_FILE:-}
HELM_REPO_NAME="cost-onprem"
HELM_REPO_URL="https://insights-onprem.github.io/cost-onprem-chart"
CHART_VERSION=${CHART_VERSION:-}  # Empty = latest; set to pin a version (e.g., "0.2.9")
USE_LOCAL_CHART=${USE_LOCAL_CHART:-false}  # Set to true to use local chart instead of Helm repository
USE_HELM_DEVEL=${USE_HELM_DEVEL:-false}  # Set to true to include pre-release (rc) charts
LOCAL_CHART_PATH=${LOCAL_CHART_PATH:-../cost-onprem}  # Path to local chart directory
KAFKA_NAMESPACE=${KAFKA_NAMESPACE:-}  # If set, look for operator and Kafka cluster in this namespace

# Logging functions with level-based filtering
log_debug() {
    [[ "$LOG_LEVEL" == "DEBUG" ]] && echo -e "${BLUE}[DEBUG]${NC} $1"
    return 0
}

log_info() {
    [[ "$LOG_LEVEL" =~ ^(INFO|DEBUG)$ ]] && echo -e "${BLUE}[INFO]${NC} $1"
    return 0
}

log_success() {
    [[ "$LOG_LEVEL" =~ ^(WARN|INFO|DEBUG)$ ]] && echo -e "${GREEN}[SUCCESS]${NC} $1"
    return 0
}

log_warning() {
    [[ "$LOG_LEVEL" =~ ^(WARN|INFO|DEBUG)$ ]] && echo -e "${YELLOW}[WARNING]${NC} $1"
    return 0
}

log_error() {
    # Errors are always shown regardless of log level
    echo -e "${RED}[ERROR]${NC} $1" >&2
    return 0
}

# Backward compatibility aliases (to be replaced incrementally)
echo_info() { log_info "$1"; }
echo_success() { log_success "$1"; }
echo_warning() { log_warning "$1"; }
echo_error() { log_error "$1"; }

# Parse S3 endpoint: strips protocol and port from FQDN
# Usage: parse_s3_host "http://s4.ns.svc.cluster.local:7480" => "s4.ns.svc.cluster.local"
parse_s3_host() {
    echo "$1" | sed -E 's|^https?://||; s|:[0-9]+/?$||; s|/$||'
}

# Extract namespace from FQDN: "s4.ns.svc.cluster.local" => "ns"
parse_s3_namespace() {
    parse_s3_host "$1" | cut -d. -f2
}

# Helm chart fullname for cost-onprem (matches templates/_helpers.tpl cost-onprem.fullname)
helm_release_fullname() {
    local chart_name="cost-onprem"
    if [[ "${HELM_RELEASE_NAME}" == *"$chart_name"* ]]; then
        echo "${HELM_RELEASE_NAME}"
    else
        echo "${HELM_RELEASE_NAME}-${chart_name}"
    fi
}

# Secret name used for S3 credentials (matches cost-onprem.storage.secretName default)
storage_credentials_secret_name() {
    echo "$(helm_release_fullname)-storage-credentials"
}

# True if endpoint hostname is AWS S3 (virtual-hosted style; do not force path-style addressing)
is_aws_s3_endpoint_host() {
    case "$1" in
        # AWS S3 endpoints
        *.amazonaws.com) return 0 ;;
        # Exclude S4 test endpoints and other local services
        s4.*svc.cluster.local|s4.*svc|*.openshift-storage.svc*) return 1 ;;
        *) return 1 ;;
    esac
}

find_explicit_s3_region() {
    if [ -n "${S3_REGION:-}" ]; then
        echo "$S3_REGION"
        return 0
    fi
    local vf v
    vf="${VALUES_FILE:-}"
    if [ -n "$vf" ] && [ -f "$vf" ] && command_exists yq; then
        v=$(yq '.objectStorage.s3.region // ""' "$vf" 2>/dev/null)
        if [ -n "$v" ] && [ "$v" != "null" ] && [ "$v" != "onprem" ]; then
            echo "$v"
            return 0
        fi
    fi
    local chart_dir="${CHART_DIR:-${SCRIPT_DIR}/../cost-onprem}"
    local base_values="${chart_dir}/values.yaml"
    if [ -f "$base_values" ] && command_exists yq; then
        v=$(yq '.objectStorage.s3.region // ""' "$base_values" 2>/dev/null)
        if [ -n "$v" ] && [ "$v" != "null" ] && [ "$v" != "onprem" ]; then
            echo "$v"
            return 0
        fi
    fi
    return 1
}

resolve_s3_cli_region() {
    find_explicit_s3_region && return 0
    echo "us-east-1"
}

# Bucket name for the S3 setup job: VALUES_FILE may be a partial overlay without
# ingress/costManagement.storage keys; yq then returns YAML null (string "null").
# Order: user values → chart values.yaml → hard-coded default (matches chart defaults).
resolve_install_bucket_name() {
    local yq_path="$1"
    local hard_default="$2"
    local chart_dir="${CHART_DIR:-${SCRIPT_DIR}/../cost-onprem}"
    local chart_values="${chart_dir}/values.yaml"
    local user_f="${VALUES_FILE:-}"
    local v=""
    local f

    for f in "$user_f" "$chart_values"; do
        [ -z "$f" ] || [ ! -f "$f" ] && continue
        command_exists yq || continue
        v=$(yq "$yq_path" "$f" 2>/dev/null || true)
        v="${v//$'\r'/}"
        v="${v#\"}"
        v="${v%\"}"
        if [ -n "$v" ] && [ "$v" != "null" ] && [ "$v" != "~" ]; then
            echo "$v"
            return 0
        fi
    done
    echo "$hard_default"
}

validate_s3_bucket_name() {
    local name="$1"
    local label="$2"
    local len=${#name}
    if [ "$len" -lt 3 ] || [ "$len" -gt 63 ]; then
        echo_error "S3 bucket name '$name' ($label) must be 3-63 characters (got $len)"
        return 1
    fi
    if echo "$name" | grep -qE '[^a-z0-9.-]'; then
        echo_error "S3 bucket name '$name' ($label) contains invalid characters (allowed: a-z, 0-9, hyphens, dots)"
        return 1
    fi
    if echo "$name" | grep -qE '^-|-$'; then
        echo_error "S3 bucket name '$name' ($label) must not start or end with a hyphen"
        return 1
    fi
    return 0
}

compute_and_export_install_bucket_names() {
    local endpoint_host="${1:-}"
    local ingress_bucket koku_bucket ros_bucket

    ingress_bucket=$(resolve_install_bucket_name '.ingress.storage.bucket' 'insights-upload-perma')
    koku_bucket=$(resolve_install_bucket_name '.costManagement.storage.bucketName' 'koku-bucket')
    ros_bucket=$(resolve_install_bucket_name '.costManagement.storage.rosBucketName' 'ros-data')

    if [ -n "${S3_BUCKET_PREFIX:-}" ]; then
        ingress_bucket="${S3_BUCKET_PREFIX}-ingress-upload"
        koku_bucket="${S3_BUCKET_PREFIX}-koku"
        ros_bucket="${S3_BUCKET_PREFIX}-ros"
    fi
    [ -n "${S3_BUCKET_INGRESS:-}" ] && ingress_bucket="$S3_BUCKET_INGRESS"
    [ -n "${S3_BUCKET_KOKU:-}" ] && koku_bucket="$S3_BUCKET_KOKU"
    [ -n "${S3_BUCKET_ROS:-}" ] && ros_bucket="$S3_BUCKET_ROS"

    local bucket_valid=true
    validate_s3_bucket_name "$ingress_bucket" "ingress" || bucket_valid=false
    validate_s3_bucket_name "$koku_bucket" "koku" || bucket_valid=false
    validate_s3_bucket_name "$ros_bucket" "ros" || bucket_valid=false
    if [ "$bucket_valid" = "false" ]; then
        echo_error "Fix bucket names via S3_BUCKET_PREFIX, S3_BUCKET_INGRESS/KOKU/ROS, or values file"
        exit 1
    fi

    if is_aws_s3_endpoint_host "$endpoint_host"; then
        if [ "$ingress_bucket" = "insights-upload-perma" ] || \
           [ "$koku_bucket" = "koku-bucket" ] || \
           [ "$ros_bucket" = "ros-data" ]; then
            echo_error "AWS S3 bucket names must be globally unique across all AWS accounts."
            echo_error "Do not use the chart defaults (insights-upload-perma, koku-bucket, ros-data) on public AWS."
            echo_error ""
            echo_error "Set a unique prefix or explicit names, for example:"
            echo_error "  export S3_BUCKET_PREFIX=\"myorg-costonprem-prod\""
            echo_error "    → buckets: myorg-costonprem-prod-ingress-upload, myorg-costonprem-prod-koku, myorg-costonprem-prod-ros"
            echo_error "Or: S3_BUCKET_INGRESS, S3_BUCKET_KOKU, S3_BUCKET_ROS (DNS-safe, lowercase)"
            echo_error "Or define ingress.storage.bucket and costManagement.storage.bucketName / rosBucketName in VALUES_FILE."
            exit 1
        fi
    fi

    export RESOLVED_S3_BUCKET_INGRESS="$ingress_bucket"
    export RESOLVED_S3_BUCKET_KOKU="$koku_bucket"
    export RESOLVED_S3_BUCKET_ROS="$ros_bucket"
}

# Read a value from the user-supplied Helm values file using yq
# Usage: get_helm_value "database.deploy" "true"
# Returns the value from VALUES_FILE if set, otherwise returns the default
get_helm_value() {
    local key="$1"
    local default="${2:-}"
    if [ -n "$VALUES_FILE" ] && [ -f "$VALUES_FILE" ]; then
        local val
        val=$(yq e ".$key" "$VALUES_FILE" 2>/dev/null)
        if [ -n "$val" ] && [ "$val" != "null" ]; then
            echo "$val"
            return
        fi
    fi
    echo "$default"
}

# Function to check if a command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Function to check prerequisites for Helm installation
check_prerequisites() {
    echo_info "Checking prerequisites for Helm chart installation..."

    local missing_tools=()

    if ! command_exists kubectl; then
        missing_tools+=("kubectl")
    fi

    if ! command_exists helm; then
        missing_tools+=("helm")
    fi

    if ! command_exists jq; then
        missing_tools+=("jq")
    fi

    if [ ${#missing_tools[@]} -gt 0 ]; then
        echo_error "Missing required tools: ${missing_tools[*]}"
        echo_info "Please install the missing tools:"

        for tool in "${missing_tools[@]}"; do
            case $tool in
                "kubectl")
                    echo_info "  Install kubectl: https://kubernetes.io/docs/tasks/tools/"
                    if [[ "$OSTYPE" == "darwin"* ]]; then
                        echo_info "  macOS: brew install kubectl"
                    fi
                    ;;
                "helm")
                    echo_info "  Install Helm: https://helm.sh/docs/intro/install/"
                    if [[ "$OSTYPE" == "darwin"* ]]; then
                        echo_info "  macOS: brew install helm"
                    fi
                    ;;
                "jq")
                    echo_info "  Install jq: https://stedolan.github.io/jq/download/"
                    if [[ "$OSTYPE" == "darwin"* ]]; then
                        echo_info "  macOS: brew install jq"
                    fi
                    ;;
            esac
        done

        return 1
    fi

    # Check kubectl context
    echo_info "Checking kubectl context..."
    local current_context=$(kubectl config current-context 2>/dev/null || echo "none")
    if [ "$current_context" = "none" ]; then
        echo_error "No kubectl context is set. Please configure kubectl to connect to your OpenShift cluster."
        echo_info "For OpenShift: oc login <cluster-url>"
        return 1
    fi

    echo_info "Current kubectl context: $current_context"

    # Test kubectl connectivity
    if ! kubectl get nodes >/dev/null 2>&1; then
        echo_error "Cannot connect to cluster. Please check your kubectl configuration."
        return 1
    fi

    echo_success "All prerequisites are met"
    return 0
}

# Function to verify OpenShift platform
detect_platform() {
    echo_info "Verifying OpenShift platform..."

    if kubectl get routes.route.openshift.io >/dev/null 2>&1; then
        echo_success "Verified OpenShift platform"
        export PLATFORM="openshift"
        # Use OpenShift values if available and no custom values specified
        if [ -z "$VALUES_FILE" ] && [ -f "$SCRIPT_DIR/../../../openshift-values.yaml" ]; then
            echo_info "Using OpenShift-specific values file"
            VALUES_FILE="$SCRIPT_DIR/../../../openshift-values.yaml"
        fi
    else
        echo_error "OpenShift platform not detected. This chart requires OpenShift."
        echo_error "Please ensure you are connected to an OpenShift cluster."
        exit 1
    fi
}

# Function to create namespace
create_namespace() {
    echo_info "Creating namespace: $NAMESPACE"

    if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
        echo_warning "Namespace '$NAMESPACE' already exists"
    else
        kubectl create namespace "$NAMESPACE"
        echo_success "Namespace '$NAMESPACE' created"
    fi

    # Apply Cost Management Metrics Operator label for resource optimization data collection
    # This label is required by the operator to collect ROS metrics from the namespace
    echo_info "Applying cost management optimization label to namespace..."
    kubectl label namespace "$NAMESPACE" cost_management_optimizations=true --overwrite
    echo_success "Cost management optimization label applied"
    echo_info "  Label: cost_management_optimizations=true"
    echo_info "  This enables the Cost Management Metrics Operator to collect resource optimization data"
}

# Function to verify AMQ Streams and Kafka prerequisites
verify_kafka() {
    echo_info "Verifying AMQ Streams operator and Kafka cluster prerequisites..."

    # If user provided external Kafka bootstrap servers (env var or values file), skip verification
    if [ -n "$KAFKA_BOOTSTRAP_SERVERS" ]; then
        echo_info "Using provided Kafka bootstrap servers: $KAFKA_BOOTSTRAP_SERVERS"
        HELM_EXTRA_ARGS+=("--set" "kafka.bootstrapServers=$KAFKA_BOOTSTRAP_SERVERS")
        echo_success "Kafka configuration verified"
        return 0
    fi

    local values_kafka_bootstrap
    values_kafka_bootstrap=$(get_helm_value "kafka.bootstrapServers" "")
    if [ -n "$values_kafka_bootstrap" ]; then
        echo_info "Using Kafka bootstrap servers from values file: $values_kafka_bootstrap"
        echo_success "Kafka configuration verified"
        return 0
    fi

    # Determine which namespace to check
    local check_namespace="${KAFKA_NAMESPACE:-kafka}"

    # Check if AMQ Streams operator exists
    local kafka_ns=""

    # Look for AMQ Streams operator in any namespace
    kafka_ns=$(kubectl get pods -A -l strimzi.io/kind=cluster-operator -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || echo "")

    if [ -n "$kafka_ns" ]; then
        echo_success "Found AMQ Streams operator in namespace: $kafka_ns"
        check_namespace="$kafka_ns"
    else
        echo_error "AMQ Streams operator not found in cluster"
        echo_info ""
        echo_info "AMQ Streams operator is required to manage Kafka clusters."
        echo_info "Please deploy AMQ Streams before installing Cost Management On Premise:"
        echo_info ""
        echo_info "  cd $SCRIPT_DIR"
        echo_info "  ./deploy-kafka.sh"
        echo_info ""
        echo_info "Or point to an existing Kafka cluster via env var or values file:"
        echo_info "  export KAFKA_BOOTSTRAP_SERVERS=my-kafka-bootstrap.my-namespace:9092"
        echo_info "  $0"
        echo_info ""
        echo_info "  # Or set kafka.bootstrapServers in your values file"
        echo_info ""
        return 1
    fi

    # Check if Kafka cluster exists
    if ! kubectl get kafka -n "$check_namespace" >/dev/null 2>&1; then
        echo_error "No Kafka cluster found in namespace: $check_namespace"
        echo_info ""
        echo_info "A Kafka cluster is required for Cost Management On Premise."
        echo_info "Please deploy a Kafka cluster before installing Cost Management On Premise:"
        echo_info ""
        echo_info "  cd $SCRIPT_DIR"
        echo_info "  ./deploy-kafka.sh"
        echo_info ""
        return 1
    fi

    # Get Kafka cluster details
    local kafka_cluster=$(kubectl get kafka -n "$check_namespace" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "$kafka_cluster" ]; then
        echo_success "Found Kafka cluster: $kafka_cluster in namespace: $check_namespace"

        # Check Kafka status
        local kafka_ready=$(kubectl get kafka "$kafka_cluster" -n "$check_namespace" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "False")
        if [ "$kafka_ready" != "True" ]; then
            echo_warning "Kafka cluster is not ready yet. Installation may fail if Kafka is not fully operational."
        fi

        # Import Kafka bootstrap servers if available from deploy-kafka.sh output
        if [ -f /tmp/kafka-bootstrap-servers.env ]; then
            source /tmp/kafka-bootstrap-servers.env
            if [ -n "$KAFKA_BOOTSTRAP_SERVERS" ]; then
                HELM_EXTRA_ARGS+=("--set" "kafka.bootstrapServers=$KAFKA_BOOTSTRAP_SERVERS")
                echo_info "Using Kafka bootstrap servers: $KAFKA_BOOTSTRAP_SERVERS"
            fi
        else
            # Fallback: auto-detect bootstrap servers
            local bootstrap_servers="${kafka_cluster}-kafka-bootstrap.${check_namespace}.svc.cluster.local:9092"
            HELM_EXTRA_ARGS+=("--set" "kafka.bootstrapServers=$bootstrap_servers")
            echo_info "Auto-detected Kafka bootstrap servers: $bootstrap_servers"
        fi
    fi

    echo_success "AMQ Streams and Kafka verification completed"
    return 0
}

# Function to detect and configure external ObjectBucketClaim (OBC)
# Used for Direct Ceph RGW deployments
detect_external_obc() {
    local obc_name="${1:-ros-data-ceph}"
    local obc_namespace="${2:-$NAMESPACE}"

    echo_info "Checking for external ObjectBucketClaim..."

    # Check if OBC exists
    if ! kubectl get obc "$obc_name" -n "$obc_namespace" >/dev/null 2>&1; then
        echo_info "No external OBC found, using standard bucket provisioning"
        return 1
    fi

    # Check if OBC is bound
    local obc_phase=$(kubectl get obc "$obc_name" -n "$obc_namespace" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [ "$obc_phase" != "Bound" ]; then
        echo_warning "External OBC '$obc_name' found but not bound (phase: $obc_phase)"
        return 1
    fi

    echo_success "✓ Detected external ObjectBucketClaim: $obc_name"

    # Extract configuration from OBC
    local bucket_name=$(kubectl get configmap "$obc_name" -n "$obc_namespace" -o jsonpath='{.data.BUCKET_NAME}' 2>/dev/null)
    local bucket_host=$(kubectl get configmap "$obc_name" -n "$obc_namespace" -o jsonpath='{.data.BUCKET_HOST}' 2>/dev/null)
    local bucket_port=$(kubectl get configmap "$obc_name" -n "$obc_namespace" -o jsonpath='{.data.BUCKET_PORT}' 2>/dev/null || echo "443")

    if [ -z "$bucket_name" ] || [ -z "$bucket_host" ]; then
        echo_error "Failed to extract bucket configuration from OBC"
        return 1
    fi

    echo_info "  Bucket Name: $bucket_name"
    echo_info "  Bucket Host: $bucket_host"
    echo_info "  Bucket Port: $bucket_port"

    # Export configuration for use in Helm deployment
    export EXTERNAL_OBC_BUCKET_NAME="$bucket_name"
    export EXTERNAL_OBC_ENDPOINT="$bucket_host"
    export EXTERNAL_OBC_PORT="$bucket_port"
    export EXTERNAL_OBC_NAME="$obc_name"

    # Create or update storage credentials secret from OBC
    # Use HELM_RELEASE_NAME (set by main()) or fall back to NAMESPACE
    local secret_name="${HELM_RELEASE_NAME:-cost-onprem}-storage-credentials"

    echo_info "Extracting credentials from OBC..."
    local access_key=$(kubectl get secret "$obc_name" -n "$obc_namespace" -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' 2>/dev/null | base64 -d)
    local secret_key=$(kubectl get secret "$obc_name" -n "$obc_namespace" -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' 2>/dev/null | base64 -d)

    if [ -z "$access_key" ] || [ -z "$secret_key" ]; then
        echo_error "Failed to extract credentials from OBC secret"
        return 1
    fi

    if kubectl get secret "$secret_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        echo_info "Storage credentials secret '$secret_name' already exists, replacing with OBC credentials..."
        kubectl delete secret "$secret_name" -n "$NAMESPACE" >/dev/null 2>&1
    fi

    echo_info "Creating storage credentials from OBC..."
    kubectl create secret generic "$secret_name" \
        -n "$NAMESPACE" \
        --from-literal=access-key="$access_key" \
        --from-literal=secret-key="$secret_key" \
        >/dev/null 2>&1

    if [ $? -eq 0 ]; then
        echo_success "✓ Storage credentials created from OBC"
    else
        echo_error "Failed to create storage credentials secret"
        return 1
    fi

    echo_info "External OBC configuration ready for deployment"
    return 0
}

# Function to create database credentials secret
# Creates a secret with credentials for all database users (postgres, ros, kruize, koku)
create_database_credentials_secret() {
    echo_info "Creating database credentials secret..."

    local secret_name="cost-onprem-db-credentials"

    # Check if secret already exists
    if kubectl get secret "$secret_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        echo_warning "Database credentials secret '$secret_name' already exists (preserving existing credentials)"
        return 0
    fi

    echo_info "Generating secure random database passwords..."

    # Generate secure random passwords (32 characters, alphanumeric)
    local postgres_password=$(openssl rand -base64 32 | tr -d "=+/" | cut -c1-32)
    local ros_password=$(openssl rand -base64 32 | tr -d "=+/" | cut -c1-32)
    local kruize_password=$(openssl rand -base64 32 | tr -d "=+/" | cut -c1-32)
    local koku_password=$(openssl rand -base64 32 | tr -d "=+/" | cut -c1-32)
    local rbac_password=$(openssl rand -base64 32 | tr -d "=+/" | cut -c1-32)
    local rbac_django_secret=$(openssl rand -base64 50 | tr -d "=+/" | cut -c1-50)

    # Create the secret
    kubectl create secret generic "$secret_name" \
        --namespace="$NAMESPACE" \
        --from-literal=postgres-user=postgres \
        --from-literal=postgres-password="$postgres_password" \
        --from-literal=ros-user=ros_user \
        --from-literal=ros-password="$ros_password" \
        --from-literal=kruize-user=kruize_user \
        --from-literal=kruize-password="$kruize_password" \
        --from-literal=koku-user=koku_user \
        --from-literal=koku-password="$koku_password" \
        --from-literal=rbac-user=rbac_user \
        --from-literal=rbac-password="$rbac_password" \
        --from-literal=django-secret-key="$rbac_django_secret"

    if [ $? -eq 0 ]; then
        echo_success "Database credentials secret created successfully"
        echo_info "  Secret: $NAMESPACE/$secret_name"
        echo_info "  📋 To retrieve credentials:"
        echo_info "    kubectl get secret $secret_name -n $NAMESPACE -o jsonpath='{.data.ros-password}' | base64 -d"
    else
        echo_error "Failed to create database credentials secret"
        return 1
    fi
}

# Function to create storage credentials secret
create_storage_credentials_secret() {
    echo_info "Creating storage credentials secret..."

    local secret_name
    secret_name=$(storage_credentials_secret_name)

    # Check if secret already exists
    if kubectl get secret "$secret_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        echo_warning "Storage credentials secret '$secret_name' already exists"
        return 0
    fi

    # Explicit credentials (AWS S3, MinIO, etc.) — highest priority when both are set
    if [ -n "${S3_ACCESS_KEY:-}" ] && [ -n "${S3_SECRET_KEY:-}" ]; then
        echo_info "Creating storage credentials from S3_ACCESS_KEY / S3_SECRET_KEY"
        kubectl create secret generic "$secret_name" \
            --namespace="$NAMESPACE" \
            --from-literal=access-key="$S3_ACCESS_KEY" \
            --from-literal=secret-key="$S3_SECRET_KEY"
        echo_success "Storage credentials created from environment variables"
        echo_info "  Storage backend: generic S3 (credentials from S3_ACCESS_KEY / S3_SECRET_KEY)"
        return 0
    fi

    # PRIORITY: If S3_ENDPOINT is set, look for S4 credentials (for testing/dev)
    if [ -n "${S3_ENDPOINT:-}" ]; then
        echo_info "S3_ENDPOINT detected: Looking for credentials..."
        local s3_ns
        s3_ns=$(parse_s3_namespace "$S3_ENDPOINT")

        # Try the S3 service namespace first, then the chart namespace
        for ns in "$s3_ns" "$NAMESPACE"; do
            if kubectl get secret s4-credentials -n "$ns" >/dev/null 2>&1; then
                echo_info "Found s4-credentials secret in namespace: $ns"
                local access_key=$(kubectl get secret s4-credentials -n "$ns" -o jsonpath='{.data.access-key}' | base64 -d)
                local secret_key=$(kubectl get secret s4-credentials -n "$ns" -o jsonpath='{.data.secret-key}' | base64 -d)
                if [ -n "$access_key" ] && [ -n "$secret_key" ]; then
                    kubectl create secret generic "$secret_name" \
                        --namespace="$NAMESPACE" \
                        --from-literal=access-key="$access_key" \
                        --from-literal=secret-key="$secret_key"
                    echo_success "Storage credentials created from S4 credentials (namespace: $ns)"
                    return 0
                fi
            fi
        done
        echo_warning "S4 credentials secret not found in $s3_ns or $NAMESPACE, falling back to default logic..."
    fi

    # OpenShift-only deployment: discover S3 credentials from known sources
    # Priority: existing S3 credentials secret > NooBaa admin > S4 > fail
    local s3_creds_secret="cost-onprem-s3-credentials"

    if kubectl get secret "$s3_creds_secret" -n "$NAMESPACE" >/dev/null 2>&1; then
        # Scenario 1: S3 credentials secret exists (created manually or by prior run)
        echo_info "Found existing S3 credentials secret: $s3_creds_secret"
        echo_info "Creating storage credentials from existing secret..."
        local access_key=$(kubectl get secret "$s3_creds_secret" -n "$NAMESPACE" -o jsonpath='{.data.access-key}')
        local secret_key=$(kubectl get secret "$s3_creds_secret" -n "$NAMESPACE" -o jsonpath='{.data.secret-key}')
        kubectl create secret generic "$secret_name" \
            --namespace="$NAMESPACE" \
            --from-literal=access-key="$(echo "$access_key" | base64 -d)" \
            --from-literal=secret-key="$(echo "$secret_key" | base64 -d)"
        echo_success "Storage credentials created from $s3_creds_secret"
    elif kubectl get secret noobaa-admin -n openshift-storage >/dev/null 2>&1; then
        # Scenario 2: NooBaa admin secret exists (ODF deployment)
        echo_info "Found noobaa-admin secret in openshift-storage namespace"
        echo_info "Extracting S3 credentials from NooBaa..."

        local access_key=$(kubectl get secret noobaa-admin -n openshift-storage -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d)
        local secret_key=$(kubectl get secret noobaa-admin -n openshift-storage -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d)

        # Cache S3 credentials for future runs
        kubectl create secret generic "$s3_creds_secret" \
            --namespace="$NAMESPACE" \
            --from-literal=access-key="$access_key" \
            --from-literal=secret-key="$secret_key"
        echo_success "Cached S3 credentials from noobaa-admin"

        # Create storage credentials secret
        kubectl create secret generic "$secret_name" \
            --namespace="$NAMESPACE" \
            --from-literal=access-key="$access_key" \
            --from-literal=secret-key="$secret_key"
        echo_success "Storage credentials created from NooBaa"
        echo_info "  Storage backend: NooBaa (via ODF)"
    elif kubectl get secret s4-credentials -n "$NAMESPACE" >/dev/null 2>&1; then
        # Scenario 3: S4 credentials exist (testing/CI environment on OpenShift)
        echo_info "Found S4 credentials secret in $NAMESPACE namespace"

        local access_key=$(kubectl get secret s4-credentials -n "$NAMESPACE" -o jsonpath='{.data.access-key}')
        local secret_key=$(kubectl get secret s4-credentials -n "$NAMESPACE" -o jsonpath='{.data.secret-key}')
        kubectl create secret generic "$secret_name" \
            --namespace="$NAMESPACE" \
            --from-literal=access-key="$(echo "$access_key" | base64 -d)" \
            --from-literal=secret-key="$(echo "$secret_key" | base64 -d)"
        echo_success "Storage credentials created from S4"
        echo_info "  Storage backend: S4 (Ceph RGW)"
    else
        # Scenario 4: No storage backend found - FAIL
        echo_error "No S3 storage credentials detected!"
        echo_error ""
        echo_error "This chart requires S3-compatible storage credentials."
        echo_error ""
        echo_info "Available options:"
        echo_info ""
        echo_info "Option 1: Export keys and re-run (AWS S3 / external S3)"
        echo_info "  export S3_ACCESS_KEY=<access-key> S3_SECRET_KEY=<secret-key>"
        echo_info "  export S3_ENDPOINT=<hostname>   # e.g. s3.us-east-1.amazonaws.com"
        echo_info "  export S3_REGION=<region>       # required for AWS (e.g. us-east-1)"
        echo_info ""
        echo_info "Option 2: Provide credentials via Kubernetes secret"
        echo_info "  kubectl create secret generic $s3_creds_secret \\"
        echo_info "      --namespace=$NAMESPACE \\"
        echo_info "      --from-literal=access-key=<your-access-key> \\"
        echo_info "      --from-literal=secret-key=<your-secret-key>"
        echo_info "  Then set S3_ENDPOINT=<hostname> when re-running this script."
        echo_info ""
        echo_info "Option 3: Configure in values.yaml (production)"
        echo_info "  - Set objectStorage.endpoint and objectStorage.secretName"
        echo_info "  - Pre-create the secret with 'access-key' and 'secret-key' keys"
        echo_info ""
        echo_info "Option 4: Deploy with S4 (Testing/CI only)"
        echo_info "  - First deploy S4: ./scripts/deploy-s4-test.sh cost-onprem"
        echo_info "  - Then re-run this installation script"
        echo_info ""
        echo_error "Deployment aborted. Please configure a storage backend and try again."
        return 1
    fi
}

# Function to create S3 buckets (required for data storage)
# This function MUST succeed for the installation to continue.
# - If buckets already exist: prints notification and continues
# - If buckets don't exist and creation fails: exits with error
#
# Environment Variables:
#   SKIP_S3_SETUP=true       - Skip bucket creation entirely (for CI/testing)
#   S3_ENDPOINT              - Manual S3 endpoint override (e.g., "s3.test.example.com")
#   S3_ACCESS_KEY            - Manual S3 access key override
#   S3_SECRET_KEY            - Manual S3 secret key override
#   S3_CLI_IMAGE             - Container image with AWS CLI (default: amazon/aws-cli:latest)
#   S3_VERIFY_SSL            - Verify TLS certificates (default: false; use true for AWS/public CAs)
#   S3_REGION                - Same as top-level docs; used for SigV4 in bucket job and Helm when set
#   S3_BUCKET_PREFIX         - Prefix for bucket names: <prefix>-ingress-upload, <prefix>-koku, <prefix>-ros
#   S3_BUCKET_INGRESS / S3_BUCKET_KOKU / S3_BUCKET_ROS - Override individual names (after prefix)
create_s3_buckets() {
    echo_info "Creating S3 buckets..."

    unset RESOLVED_S3_BUCKET_INGRESS RESOLVED_S3_BUCKET_KOKU RESOLVED_S3_BUCKET_ROS 2>/dev/null || true

    # Check if S3 setup should be skipped entirely
    if [ "${SKIP_S3_SETUP:-false}" = "true" ]; then
        echo_info "Skipping S3 bucket creation (SKIP_S3_SETUP=true)"
        echo_info "This is typically used in CI environments or when S3 is managed externally"
        return 0
    fi

    local secret_name
    secret_name=$(storage_credentials_secret_name)

    # Get S3 credentials from environment variables or secret
    local access_key="${S3_ACCESS_KEY:-}"
    local secret_key="${S3_SECRET_KEY:-}"

    # If credentials not provided via environment variables, get from secret
    if [ -z "$access_key" ] || [ -z "$secret_key" ]; then
        echo_info "Getting S3 credentials from secret: $secret_name"
        access_key=$(kubectl get secret "$secret_name" -n "$NAMESPACE" -o jsonpath='{.data.access-key}' 2>/dev/null | base64 -d)
        secret_key=$(kubectl get secret "$secret_name" -n "$NAMESPACE" -o jsonpath='{.data.secret-key}' 2>/dev/null | base64 -d)
    else
        echo_info "Using S3 credentials from environment variables"
    fi

    if [ -z "$access_key" ] || [ -z "$secret_key" ]; then
        # Check if this might be a CI/S4 environment where credentials are managed differently
        if [ -n "${S3_ENDPOINT:-}" ] && [[ "$S3_ENDPOINT" == *"svc.cluster.local"* ]]; then
            echo_warning "S3 credentials not found for cluster-local endpoint: $S3_ENDPOINT"
            echo_warning "This may be a CI/S4 environment. Skipping bucket creation."
            echo_info "If buckets don't exist, they may need to be created manually or by S4 setup."
            return 0
        fi

        echo_error "S3 credentials not found in secret $secret_name or environment variables"
        echo_error "Cannot proceed without S3 storage. Aborting installation."
        echo_error ""
        echo_error "To bypass this requirement for CI environments, set:"
        echo_error "  export SKIP_S3_SETUP=true"
        exit 1
    fi

    # Determine S3 endpoint and configuration
    # Priority: S3_ENDPOINT env > NooBaa > VALUES_FILE objectStorage > chart values.yaml
    local s3_url no_verify_ssl endpoint_host="" aws_addr_style="path"

    # Default to skipping verification: most on-prem S3 backends use certs
    # signed by the cluster service CA, which is not in the container's trust
    # store.  Set S3_VERIFY_SSL=true when the endpoint has a publicly trusted
    # certificate (e.g., AWS S3 or a corporate CA in a custom image).
    local verify_ssl="${S3_VERIFY_SSL:-false}"

    if [ -n "${S3_ENDPOINT:-}" ]; then
        # Explicit S3_ENDPOINT (AWS, S4, NooBaa override, etc.)
        endpoint_host=$(parse_s3_host "$S3_ENDPOINT")

        # Auto-enable SSL verification for AWS endpoints unless explicitly disabled
        if is_aws_s3_endpoint_host "$endpoint_host" && [ -z "${S3_VERIFY_SSL:-}" ]; then
            verify_ssl="true"
            echo_info "  Auto-enabled SSL verification for AWS S3 endpoint (override with S3_VERIFY_SSL=false)"
        fi
        local s3_port="${S3_PORT:-$(echo "$S3_ENDPOINT" | grep -oP ':\K[0-9]+$' || echo 443)}"
        local s3_ssl="${S3_USE_SSL:-true}"
        if [ "$s3_ssl" = "true" ]; then
            s3_url="https://${endpoint_host}:${s3_port}"
            [ "$verify_ssl" = "true" ] && no_verify_ssl="" || no_verify_ssl="--no-verify-ssl"
        else
            s3_url="http://${endpoint_host}:${s3_port}"
            no_verify_ssl=""
        fi
        echo_info "  ✓ Using S3_ENDPOINT: $s3_url"
    elif kubectl get crd noobaas.noobaa.io >/dev/null 2>&1 && \
       kubectl get noobaa -n openshift-storage >/dev/null 2>&1; then
        # NooBaa auto-detection (ODF S3 backend)
        endpoint_host="s3.openshift-storage.svc"
        s3_url="https://s3.openshift-storage.svc:443"
        no_verify_ssl="--no-verify-ssl"
        echo_info "  ✓ Detected: NooBaa S3 (via ODF)"
    else
        # Read objectStorage from user values file first, then bundled chart values.yaml
        local chart_dir="${CHART_DIR:-${SCRIPT_DIR}/../cost-onprem}"
        local base_values="${chart_dir}/values.yaml"
        local s3_ep="" s3_port="443" s3_ssl="true"
        local src_values=""
        if [ -n "${VALUES_FILE:-}" ] && [ -f "$VALUES_FILE" ] && command_exists yq; then
            src_values="$VALUES_FILE"
        elif [ -f "$base_values" ]; then
            src_values="$base_values"
        fi
        if [ -n "$src_values" ] && command_exists yq; then
            s3_ep=$(yq '.objectStorage.endpoint // ""' "$src_values" 2>/dev/null)
            s3_port=$(yq '.objectStorage.port // 443' "$src_values" 2>/dev/null)
            s3_ssl=$(yq '.objectStorage.useSSL // true' "$src_values" 2>/dev/null)
        fi
        if [ -n "$s3_ep" ] && [ "$s3_ep" != "null" ]; then
            endpoint_host=$(parse_s3_host "$s3_ep")

            if is_aws_s3_endpoint_host "$endpoint_host" && [ -z "${S3_VERIFY_SSL:-}" ]; then
                verify_ssl="true"
                echo_info "  Auto-enabled SSL verification for AWS S3 endpoint (override with S3_VERIFY_SSL=false)"
            fi

            if [ "$s3_ssl" = "true" ]; then
                s3_url="https://${s3_ep}:${s3_port}"
                [ "$verify_ssl" = "true" ] && no_verify_ssl="" || no_verify_ssl="--no-verify-ssl"
            else
                s3_url="http://${s3_ep}:${s3_port}"
                no_verify_ssl=""
            fi
            echo_info "  ✓ Using S3 from values (${src_values##*/}): $s3_url"
        else
            echo_error "Could not detect S3 storage backend"
            echo_error "Checked for: S3_ENDPOINT env var, NooBaa CRD, values objectStorage.endpoint"
            echo_error ""
            echo_error "Solutions:"
            echo_error "  1. AWS S3: S3_ENDPOINT=s3.<region>.amazonaws.com S3_REGION=<region> S3_ACCESS_KEY=... S3_SECRET_KEY=..."
            echo_error "  2. Set S3_ENDPOINT for any S3-compatible endpoint (optional: S3_PORT, S3_USE_SSL)"
            echo_error "  3. Set objectStorage.endpoint in VALUES_FILE or ensure chart values.yaml is available"
            echo_error "  4. Deploy S4 for dev/test: ./scripts/deploy-s4-test.sh cost-onprem"
            echo_error "  5. Set SKIP_S3_SETUP=true to skip bucket creation"
            exit 1
        fi
    fi

    local aws_region
    if is_aws_s3_endpoint_host "$endpoint_host"; then
        aws_addr_style="auto"
        echo_info "  AWS S3 endpoint: using virtual-hosted-style (addressing_style=auto) for AWS CLI"

        local explicit_region
        if ! explicit_region=$(find_explicit_s3_region); then
            echo_error "Region is required for AWS S3 endpoints (SigV4 authentication)."
            echo_error ""
            echo_error "Provide the region via environment variable or values file:"
            echo_error "  export S3_REGION=us-east-1"
            echo_error "Or in your values file:"
            echo_error "  objectStorage:"
            echo_error "    s3:"
            echo_error "      region: us-east-1"
            echo_error ""
            echo_error "Common AWS regions: us-east-1, us-west-2, eu-west-1, ap-southeast-1"
            exit 1
        fi
        aws_region="$explicit_region"

        local expected_region=""
        case "$endpoint_host" in
            s3.dualstack.*.amazonaws.com)
                expected_region=$(echo "$endpoint_host" | sed 's/s3\.dualstack\.\([^.]*\)\.amazonaws\.com/\1/')
                ;;
            s3-*.*.amazonaws.com)
                expected_region=$(echo "$endpoint_host" | sed 's/s3-[^.]*\.\([^.]*\)\.amazonaws\.com/\1/')
                ;;
            s3.*.amazonaws.com)
                expected_region=$(echo "$endpoint_host" | sed 's/s3\.\([^.]*\)\.amazonaws\.com/\1/')
                ;;
        esac
        if [ -n "$expected_region" ] && [ "$expected_region" != "$aws_region" ]; then
            echo_warning "Region mismatch: resolved region=${aws_region} but endpoint suggests ${expected_region}"
            echo_warning "This may cause SigV4 authentication failures"
        fi
        echo_info "  AWS CLI region for bucket job: $aws_region"
    else
        aws_region=$(resolve_s3_cli_region)
    fi

    compute_and_export_install_bucket_names "$endpoint_host"
    local bucket_list="${RESOLVED_S3_BUCKET_INGRESS} ${RESOLVED_S3_BUCKET_KOKU} ${RESOLVED_S3_BUCKET_ROS}"
    echo_info "Creating buckets at ${s3_url}..."

    local run_id
    run_id=$(date +%s)
    local temp_secret_name="${secret_name}-bucket-job-${run_id}"
    local bucket_pod_name="bucket-setup-${run_id}"
    local bucket_image="${S3_CLI_IMAGE:-amazon/aws-cli:latest}"

    cleanup_bucket_job() {
        kubectl delete pod "$bucket_pod_name" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
        kubectl delete secret "$temp_secret_name" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
    }

    cleanup_bucket_job
    echo_info "Creating credentials secret for bucket creation job..."
    kubectl create secret generic "$temp_secret_name" \
        --namespace="$NAMESPACE" \
        --from-literal=access-key="$access_key" \
        --from-literal=secret-key="$secret_key"

    trap cleanup_bucket_job EXIT INT TERM

    kubectl create -f - <<PODEOF
apiVersion: v1
kind: Pod
metadata:
  name: ${bucket_pod_name}
  namespace: ${NAMESPACE}
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 1001
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: bucket-setup
    image: ${bucket_image}
    env:
    - name: AWS_ACCESS_KEY_ID
      valueFrom:
        secretKeyRef:
          name: ${temp_secret_name}
          key: access-key
    - name: AWS_SECRET_ACCESS_KEY
      valueFrom:
        secretKeyRef:
          name: ${temp_secret_name}
          key: secret-key
    - name: AWS_DEFAULT_REGION
      value: "${aws_region}"
    - name: HOME
      value: "/tmp"
    command: ["sh", "-c"]
    args:
    - |
      set -e
      mkdir -p /tmp/.aws
      printf '[default]\ns3 =\n    addressing_style = ${aws_addr_style}\n' > /tmp/.aws/config
      for bucket in ${bucket_list}; do
        if aws s3api head-bucket --bucket \${bucket} --endpoint-url ${s3_url} ${no_verify_ssl} 2>/dev/null; then
          echo "Bucket \${bucket} already exists"
        else
          aws s3 mb s3://\${bucket} --endpoint-url ${s3_url} ${no_verify_ssl}
          echo "Created bucket: \${bucket}"
        fi
      done
      echo ''
      echo 'Available buckets:'
      aws s3 ls --endpoint-url ${s3_url} ${no_verify_ssl}
PODEOF

    echo_info "Waiting for bucket creation job to complete..."
    local output pod_phase
    if kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
        pod/"$bucket_pod_name" -n "$NAMESPACE" --timeout=300s 2>/dev/null; then
        output=$(kubectl logs "$bucket_pod_name" -n "$NAMESPACE" 2>&1 || true)
        echo "$output"
        echo_success "S3 buckets ready"
        cleanup_bucket_job
        trap - EXIT INT TERM
        return 0
    else
        pod_phase=$(kubectl get pod "$bucket_pod_name" -n "$NAMESPACE" \
            -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
        output=$(kubectl logs "$bucket_pod_name" -n "$NAMESPACE" 2>&1 || echo "No logs available")
        echo "$output"
        echo_error "Failed to create S3 buckets (pod phase: $pod_phase). Cannot proceed without storage."
        echo_error "Check S3 connectivity and credentials."
        cleanup_bucket_job
        trap - EXIT INT TERM
        exit 1
    fi
}

# Function to set up the Helm chart repository
setup_helm_repo() {
    echo_info "Setting up Helm chart repository..."

    # Add or update the Helm repo
    if helm repo list 2>/dev/null | grep -q "^${HELM_REPO_NAME}"; then
        echo_info "Updating existing Helm repo '${HELM_REPO_NAME}'..."
        helm repo update "${HELM_REPO_NAME}"
    else
        echo_info "Adding Helm repo '${HELM_REPO_NAME}' from ${HELM_REPO_URL}..."
        if ! helm repo add "${HELM_REPO_NAME}" "${HELM_REPO_URL}"; then
            echo_error "Failed to add Helm repository"
            echo_info "Verify the repository URL is accessible: ${HELM_REPO_URL}"
            return 1
        fi
        helm repo update "${HELM_REPO_NAME}"
    fi

    # Verify chart is available
    local search_output
    if ! search_output=$(helm search repo "${HELM_REPO_NAME}/cost-onprem" --output json 2>/dev/null); then
        echo_error "Chart 'cost-onprem' not found in repository"
        return 1
    fi

    local available_version
    available_version=$(echo "$search_output" | jq -r '.[0].version // empty')
    if [ -z "$available_version" ]; then
        echo_error "No versions of chart 'cost-onprem' found in repository"
        return 1
    fi
    echo_success "Helm repo configured. Latest available chart version: ${available_version}"

    return 0
}


# Pre-flight validation: warn about cluster-specific values that could not be
# auto-detected. These were previously discovered at render time via lookup();
# now the install script must supply them via --set.
preflight_validate() {
    local warnings=0

    echo_info "Pre-flight validation..."

    # Cluster domain (required for Route hostnames)
    if [ "$PLATFORM" = "openshift" ]; then
        local cluster_domain
        cluster_domain=$(oc get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)
        if [ -z "$cluster_domain" ]; then
            echo_warning "Could not detect cluster domain (ingress.config.openshift.io/cluster)"
            echo_info "  Routes will use default 'apps.cluster.local' — override with --set global.clusterDomain=..."
            warnings=$((warnings + 1))
        fi
    fi

    # S3 / Object Storage endpoint
    if [ "$USER_S3_CONFIGURED" != "true" ] && [ "$USING_EXTERNAL_OBC" != "true" ] && \
       [ -z "${S3_ENDPOINT:-}" ]; then
        # NooBaa detection
        if ! kubectl get crd noobaas.noobaa.io >/dev/null 2>&1 || \
           ! kubectl get noobaa -n openshift-storage >/dev/null 2>&1; then
            echo_warning "No in-cluster ODF/NooBaa S3 detected (OBC, S3_ENDPOINT, or NooBaa)"
            echo_info "  For AWS S3 set S3_ENDPOINT (e.g. s3.us-east-1.amazonaws.com) and S3_REGION"
            echo_info "  Otherwise chart defaults to 's3.openshift-storage.svc.cluster.local'"
            echo_info "  Override with --set objectStorage.endpoint=... or a VALUES_FILE"
            warnings=$((warnings + 1))
        fi
    fi

    # Keycloak (required for JWT authentication)
    if [ "$PLATFORM" = "openshift" ] && [ "$KEYCLOAK_FOUND" != "true" ]; then
        echo_warning "RHBK (Keycloak) not detected — JWT authentication may not work"
        echo_info "  Deploy RHBK first or override: --set jwtAuth.keycloak.url=..."
        warnings=$((warnings + 1))
    fi

    if [ $warnings -gt 0 ]; then
        echo_warning "Pre-flight: $warnings warning(s) — chart defaults will be used for missing values"
    else
        echo_success "Pre-flight validation passed"
    fi

    # Pre-flight is advisory; always return success
    return 0
}

# Function to deploy Helm chart
deploy_helm_chart() {
    echo_info "Deploying Cost Management On Premise Helm chart..."

    local project_root
    project_root="$(cd "${SCRIPT_DIR}/.." && pwd)"
    local crd_name="costmanagementserviceconfigs.service.costmanagement.openshift.io"
    local sample="${project_root}/config/samples/service.costmanagement_v1alpha1_costmanagementserviceconfig.yaml"

    local operator_ns="${NAMESPACE:-cost-onprem}"
    local pull_img="${IMG:-image-registry.openshift-image-registry.svc:5000/${operator_ns}/koku-service-operator:latest}"
    local registry_host
    registry_host="$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}' 2>/dev/null || true)"
    if [ -z "$registry_host" ]; then
        echo_info "Enabling OpenShift registry default-route"
        oc patch configs.imageregistry.operator.openshift.io/cluster --type merge -p '{"spec":{"defaultRoute":true}}' >/dev/null
        local _i
        for _i in $(seq 1 60); do
            registry_host="$(oc get route default-route -n openshift-image-registry -o jsonpath='{.spec.host}' 2>/dev/null || true)"
            [ -n "$registry_host" ] && break
            sleep 2
        done
    fi
    if [ -z "$registry_host" ]; then
        echo_error "OpenShift registry default-route not available"
        return 1
    fi
    local build_img="${registry_host}/${operator_ns}/koku-service-operator:latest"
    kubectl get ns "$operator_ns" >/dev/null 2>&1 || kubectl create ns "$operator_ns"
    echo_info "Building and pushing operator image: $build_img"
    if ! (cd "$project_root" && make docker-build IMG="$build_img" && { oc registry login --registry="$registry_host" 2>/dev/null || true; } && oc whoami -t | docker login -u "$(oc whoami)" --password-stdin "$registry_host" && make docker-push IMG="$build_img"); then
        echo_error "Failed to build/push operator image"
        return 1
    fi
    echo_info "CMSC CRD not found — deploying operator (make deploy)"
    if ! (cd "$project_root" && make deploy IMG="$pull_img"); then
        echo_error "Failed to deploy operator/CRDs via make deploy"
        return 1
    fi
    # TODO: drop once wait-for init images are not pulled from the operator ImageStream across namespaces
    echo_info "Granting image-puller so ${NAMESPACE} SAs can pull wait-for init image from ${operator_ns}"
    oc adm policy add-role-to-group system:image-puller "system:serviceaccounts:${NAMESPACE}" -n "$operator_ns" || true
    echo_success "Operator/CRDs deployed"

    if [ ! -f "$sample" ]; then
        echo_error "CMSC sample not found: $sample"
        return 1
    fi

    # TODO: remove anyuid SCC grant after operator sets compatible securityContext for bundled DB/cache (fsGroup 26 / PodSecurity)
    echo_info "Granting anyuid SCC for bundled DB/cache pods"
    oc adm policy add-scc-to-user anyuid -z default -n "${NAMESPACE:-cost-onprem}" 2>/dev/null || true

    echo_info "Applying CMSC sample: $sample"
    if ! kubectl apply -f "$sample"; then
        echo_error "Failed to apply CMSC"
        return 1
    fi

    # RHBK advertises the public Route as OIDC issuer even when JWKS uses the
    # in-cluster Service URL. Patch issuerURL from the detected Keycloak hostname
    # so oauth2-proxy / Envoy match tokens without hardcoding a cluster URL in the sample.
    local cr_name="${CR_NAME:-cost-onprem}"
    if [ -z "${KEYCLOAK_URL:-}" ]; then
        detect_keycloak || true
    fi
    if [ -n "${KEYCLOAK_URL:-}" ]; then
        echo_info "Patching CMSC ${cr_name} spec.auth.keycloak.issuerURL=${KEYCLOAK_URL}"
        if ! kubectl patch cmsc "$cr_name" -n "$NAMESPACE" --type merge \
            -p "{\"spec\":{\"auth\":{\"keycloak\":{\"issuerURL\":\"${KEYCLOAK_URL}\"}}}}"; then
            echo_warning "Failed to patch issuerURL; UI OIDC discovery may fail until set manually"
        fi
    else
        echo_warning "KEYCLOAK_URL unset — skipping issuerURL patch (set auth.keycloak.issuerURL if UI OIDC fails)"
    fi

    echo_success "Helm chart deployed successfully"
    return 0

    local chart_source=""

    # Determine chart source
    if [ "$USE_LOCAL_CHART" = "true" ]; then
        echo_info "Using local chart source (USE_LOCAL_CHART=true)"
        cd "$SCRIPT_DIR"

        # Check if Helm chart directory exists
        if [ ! -d "$LOCAL_CHART_PATH" ]; then
            echo_error "Local Helm chart directory not found: $LOCAL_CHART_PATH"
            echo_info "Set USE_LOCAL_CHART=false to use the Helm repository, or set LOCAL_CHART_PATH to the correct chart location (default: ./cost-onprem)"
            return 1
        fi

        chart_source="$LOCAL_CHART_PATH"
        echo_info "Using local chart: $chart_source"
    else
        echo_info "Using Helm repository (USE_LOCAL_CHART=false)"

        if ! setup_helm_repo; then
            echo_error "Failed to set up Helm repository"
            echo_info "Fallback: Set USE_LOCAL_CHART=true to use local chart"
            return 1
        fi

        chart_source="${HELM_REPO_NAME}/cost-onprem"
        echo_info "Using Helm repository chart: $chart_source"
    fi

    # Build Helm command
    local helm_cmd="helm upgrade --install \"$HELM_RELEASE_NAME\" \"$chart_source\""
    helm_cmd="$helm_cmd --namespace \"$NAMESPACE\""
    helm_cmd="$helm_cmd --create-namespace"
    helm_cmd="$helm_cmd --timeout=${HELM_TIMEOUT:-600s}"
    helm_cmd="$helm_cmd --wait"

    # Pin chart version if specified (only for Helm repo, not local chart)
    if [ -n "$CHART_VERSION" ] && [ "$USE_LOCAL_CHART" != "true" ]; then
        helm_cmd="$helm_cmd --version \"$CHART_VERSION\""
        echo_info "Pinning chart version: $CHART_VERSION"
    fi

    # Include pre-release charts (e.g., rc versions) when --devel is enabled
    if [ "$USE_HELM_DEVEL" = "true" ] && [ "$USE_LOCAL_CHART" != "true" ]; then
        helm_cmd="$helm_cmd --devel"
        echo_info "Including pre-release (--devel) charts"
    fi

    # Add values file if specified
    if [ -n "$VALUES_FILE" ]; then
        if [ -f "$VALUES_FILE" ]; then
            echo_info "Using values file: $VALUES_FILE"
            helm_cmd="$helm_cmd -f \"$VALUES_FILE\""
        else
            echo_error "Values file not found: $VALUES_FILE"
            return 1
        fi
    fi

    # Add sizing profile overlay (applied after VALUES_FILE so it wins on merge)
    if [ -n "${SIZING_VALUES_FILE:-}" ]; then
        if [ -f "$SIZING_VALUES_FILE" ]; then
            echo_info "Using sizing profile overlay: $SIZING_VALUES_FILE"
            helm_cmd="$helm_cmd -f \"$SIZING_VALUES_FILE\""
        else
            echo_error "Sizing profile overlay not found: $SIZING_VALUES_FILE"
            return 1
        fi
    fi

    # -------------------------------------------------------------------------
    # Cluster-detected values (FLPATH-3181: chart no longer uses lookup())
    # The install script is the single source of truth for cluster-specific
    # values. All detection that previously happened at Helm render time via
    # lookup() is now done here and passed via --set.
    # -------------------------------------------------------------------------

    # Cluster domain (for Route hostnames)
    if [ "$PLATFORM" = "openshift" ]; then
        local cluster_domain
        cluster_domain=$(oc get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)
        if [ -n "$cluster_domain" ]; then
            helm_cmd="$helm_cmd --set global.clusterDomain=\"$cluster_domain\""
            echo_info "Cluster domain: $cluster_domain"
        fi
    fi

    # Storage class (auto-detect default)
    local detected_sc
    detected_sc=$(kubectl get sc -o jsonpath='{.items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")].metadata.name}' 2>/dev/null | awk '{print $1}' || true)
    if [ -n "$detected_sc" ]; then
        helm_cmd="$helm_cmd --set global.storageClass=\"$detected_sc\""
        echo_info "Storage class: $detected_sc"
    fi

    # Valkey fsGroup (from namespace supplemental-groups annotation)
    # Only relevant when the chart deploys bundled Valkey
    local valkey_deploy
    valkey_deploy=$(get_helm_value "valkey.deploy" "true")
    if [ "$valkey_deploy" != "false" ] && [ "$PLATFORM" = "openshift" ]; then
        local supp_groups
        supp_groups=$(oc get ns "$NAMESPACE" -o jsonpath='{.metadata.annotations.openshift\.io/sa\.scc\.supplemental-groups}' 2>/dev/null || true)
        if [ -n "$supp_groups" ]; then
            # Extract first number from "1000740000/10000" format
            local fs_group
            fs_group=$(echo "$supp_groups" | cut -d'/' -f1)
            if [ -n "$fs_group" ]; then
                helm_cmd="$helm_cmd --set valkey.securityContext.fsGroup=$fs_group"
                echo_info "Valkey fsGroup: $fs_group"
            fi
        fi
    fi

    # Keycloak values (chart no longer uses lookup() for Keycloak detection)
    if [ "$PLATFORM" = "openshift" ] && [ "$KEYCLOAK_FOUND" = "true" ]; then
        helm_cmd="$helm_cmd --set jwtAuth.keycloak.installed=true"
        if [ -n "$KEYCLOAK_NAMESPACE" ]; then
            helm_cmd="$helm_cmd --set jwtAuth.keycloak.namespace=\"$KEYCLOAK_NAMESPACE\""
        fi
        if [ -n "$KEYCLOAK_URL" ]; then
            helm_cmd="$helm_cmd --set jwtAuth.keycloak.url=\"$KEYCLOAK_URL\""
        fi
        echo_info "Keycloak: installed=true namespace=$KEYCLOAK_NAMESPACE url=${KEYCLOAK_URL:-auto}"
    elif [ "$PLATFORM" = "openshift" ]; then
        echo_warning "RHBK not detected — Keycloak values will use chart defaults"
    fi

    # S3 endpoint configuration for Helm:
    # If user pre-configured S3 in values.yaml, skip all --set overrides
    # (the values file already has the right config).
    # Otherwise, auto-inject from OBC detection or S3_ENDPOINT.
    if [ "$USER_S3_CONFIGURED" = "true" ]; then
        echo_info "S3 configuration provided in values file — skipping Helm --set overrides"
    elif [ "$USING_EXTERNAL_OBC" = "true" ]; then
        echo_info "Configuring Helm deployment for external OBC (Direct Ceph RGW)"
        helm_cmd="$helm_cmd --set objectStorage.endpoint=\"$EXTERNAL_OBC_ENDPOINT\""
        helm_cmd="$helm_cmd --set objectStorage.port=\"$EXTERNAL_OBC_PORT\""
        helm_cmd="$helm_cmd --set objectStorage.useSSL=true"
        helm_cmd="$helm_cmd --set ingress.storage.bucket=\"$EXTERNAL_OBC_BUCKET_NAME\""

        # Set bucket names for Koku and ROS (via standardized helpers in _helpers.tpl)
        helm_cmd="$helm_cmd --set costManagement.storage.bucketName=\"$EXTERNAL_OBC_BUCKET_NAME\""
        helm_cmd="$helm_cmd --set costManagement.storage.rosBucketName=\"$EXTERNAL_OBC_BUCKET_NAME\""

        # Also set ROS bucket name for ROS components
        helm_cmd="$helm_cmd --set ros.storage.bucketName=\"$EXTERNAL_OBC_BUCKET_NAME\""

        echo_success "✓ External OBC configuration added to Helm deployment"
        echo_info "  Endpoint: https://$EXTERNAL_OBC_ENDPOINT:$EXTERNAL_OBC_PORT"
        echo_info "  Bucket: $EXTERNAL_OBC_BUCKET_NAME"
        echo_info "  Bucket configured for ingress, Koku, and ROS components"
    elif [ -n "${S3_ENDPOINT:-}" ]; then
        # Explicit S3_ENDPOINT env var (AWS S3, S4, NooBaa override, etc.)
        local s3_port="${S3_PORT:-443}"
        local s3_ssl="${S3_USE_SSL:-true}"
        echo_info "Configuring S3 endpoint from S3_ENDPOINT env var"
        helm_cmd="$helm_cmd --set objectStorage.endpoint=\"${S3_ENDPOINT}\""
        helm_cmd="$helm_cmd --set objectStorage.port=${s3_port}"
        helm_cmd="$helm_cmd --set objectStorage.useSSL=${s3_ssl}"
        if [ -n "${S3_REGION:-}" ]; then
            helm_cmd="$helm_cmd --set objectStorage.s3.region=\"${S3_REGION}\""
            echo_info "objectStorage.s3.region=${S3_REGION} (AWS S3 / SigV4)"
        fi
        local deploy_s3_host
        deploy_s3_host=$(parse_s3_host "${S3_ENDPOINT}")
        if is_aws_s3_endpoint_host "$deploy_s3_host"; then
            helm_cmd="$helm_cmd --set objectStorage.s3.addressingStyle=auto"
            echo_info "objectStorage.s3.addressingStyle=auto (AWS S3 virtual-hosted style)"
        fi
        echo_success "✓ S3 endpoint configured: ${S3_ENDPOINT} (port ${s3_port}, SSL=${s3_ssl})"
    elif kubectl get crd noobaas.noobaa.io >/dev/null 2>&1 && \
         kubectl get noobaa -n openshift-storage >/dev/null 2>&1; then
        # NooBaa fallback (ODF S3 backend)
        echo_info "Configuring S3 endpoint for NooBaa (ODF)"
        helm_cmd="$helm_cmd --set objectStorage.endpoint=\"s3.openshift-storage.svc\""
        helm_cmd="$helm_cmd --set objectStorage.port=443"
        helm_cmd="$helm_cmd --set objectStorage.useSSL=true"
        echo_success "✓ S3 endpoint configured: s3.openshift-storage.svc (port 443, SSL)"
    fi

    # Bucket names from create_s3_buckets (AWS/global S3 uniqueness, S3_BUCKET_* env, or chart defaults).
    if [ "$USER_S3_CONFIGURED" != "true" ] && [ -n "${RESOLVED_S3_BUCKET_INGRESS:-}" ]; then
        helm_cmd="$helm_cmd --set ingress.storage.bucket=\"${RESOLVED_S3_BUCKET_INGRESS}\""
        helm_cmd="$helm_cmd --set costManagement.storage.bucketName=\"${RESOLVED_S3_BUCKET_KOKU}\""
        helm_cmd="$helm_cmd --set costManagement.storage.rosBucketName=\"${RESOLVED_S3_BUCKET_ROS}\""
        echo_info "Helm bucket names: ${RESOLVED_S3_BUCKET_INGRESS}, ${RESOLVED_S3_BUCKET_KOKU}, ${RESOLVED_S3_BUCKET_ROS}"
    fi

    # Tell Helm about the script-managed storage credentials secret so it
    # skips rendering the placeholder secret template (avoids ownership conflict).
    if [ -n "$STORAGE_CREDENTIALS_SECRET" ]; then
        helm_cmd="$helm_cmd --set objectStorage.secretName=\"$STORAGE_CREDENTIALS_SECRET\""
        echo_info "Storage credentials secret: $STORAGE_CREDENTIALS_SECRET (script-managed)"
    fi

    # Add additional Helm arguments passed to the script
    if [ ${#HELM_EXTRA_ARGS[@]} -gt 0 ]; then
        echo_info "Adding additional Helm arguments: ${HELM_EXTRA_ARGS[*]}"
        helm_cmd="$helm_cmd ${HELM_EXTRA_ARGS[*]}"
    fi

    # Reconcile server-side apply conflicts (e.g. after oc set image on deployments)
    if [ "${HELM_FORCE_CONFLICTS:-false}" = "true" ]; then
        helm_cmd="$helm_cmd --force-conflicts"
        echo_info "Helm upgrade: --force-conflicts (server-side apply)"
    fi

    echo_info "Executing: $helm_cmd"

    # Execute Helm command
    eval $helm_cmd

    local helm_exit_code=$?

    if [ $helm_exit_code -eq 0 ]; then
        echo_success "Helm chart deployed successfully"
    else
        echo_error "Failed to deploy Helm chart"
        return 1
    fi
}

# Function to wait for pods to be ready
wait_for_pods() {
    echo_info "Waiting for pods to be ready..."

    # Wait for CMSC Ready (replaces Helm release pod wait)
    local cr_name="${CR_NAME:-cost-onprem}"
    local cr_ns="${NAMESPACE:-cost-onprem}"
    local timeout_s="${HELM_TIMEOUT:-900}"
    timeout_s="${timeout_s%s}"
    local end=$((SECONDS + timeout_s))
    local phase=""
    while (( SECONDS < end )); do
        phase="$(kubectl get cmsc "$cr_name" -n "$cr_ns" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        if [ "$phase" = "Ready" ]; then
            break
        fi
        echo_info "CMSC phase=${phase:-<empty>} (waiting for Ready)"
        sleep 10
    done
    if [ "$phase" != "Ready" ]; then
        echo_error "CMSC did not become Ready within ${timeout_s}s"
        kubectl get cmsc "$cr_name" -n "$cr_ns" -o yaml 2>/dev/null || true
        return 1
    fi

    # Wait for all pods to be ready (excluding jobs) with extended timeout for full deployment
    kubectl wait --for=condition=ready pod -l "app.kubernetes.io/instance=$cr_name" \
        --namespace "$cr_ns" \
        --timeout=900s \
        --field-selector=status.phase!=Succeeded

    echo_success "All pods are ready"
}

# Trigger the Keycloak-to-RBAC sync CronJob and wait for its first run to
# complete.  Only runs when rbac.keycloakSync.enabled=true in the Helm values.
trigger_keycloak_sync() {
    local sync_enabled
    sync_enabled=$(get_helm_value "rbac.keycloakSync.enabled" "false")
    if [ "$sync_enabled" != "true" ]; then
        return 0
    fi

    local cronjob_name="${HELM_RELEASE_NAME}-rbac-keycloak-sync"
    local job_name="${cronjob_name}-initial-$(date +%s)"

    echo_info "Triggering initial Keycloak-to-RBAC user sync..."

    if ! kubectl get cronjob "$cronjob_name" -n "$NAMESPACE" &>/dev/null; then
        echo_warning "CronJob $cronjob_name not found — skipping initial sync"
        return 0
    fi

    kubectl create job "$job_name" --from="cronjob/$cronjob_name" -n "$NAMESPACE"

    echo_info "Waiting for sync job $job_name to complete (timeout 300s)..."
    if kubectl wait --for=condition=complete "job/$job_name" \
        -n "$NAMESPACE" --timeout=300s 2>/dev/null; then
        echo_success "Keycloak-to-RBAC user sync completed successfully"
        kubectl logs "job/$job_name" -n "$NAMESPACE" --tail=20 2>/dev/null || true
    else
        echo_warning "Sync job did not complete within 300s"
        echo_info "Check logs: kubectl logs job/$job_name -n $NAMESPACE"
        echo_info "The CronJob will retry automatically on schedule"
    fi
}

# Function to show deployment status
show_status() {
    echo_info "Deployment Status"
    echo_info "=================="

    echo_info "Platform: $PLATFORM"
    echo_info "Namespace: $NAMESPACE"
    echo_info "Helm Release: $HELM_RELEASE_NAME"
    if [ -n "$VALUES_FILE" ]; then
        echo_info "Values File: $VALUES_FILE"
    fi
    echo ""

    echo_info "Pods:"
    kubectl get pods -n "$NAMESPACE" -o wide
    echo ""

    echo_info "Services:"
    kubectl get services -n "$NAMESPACE"
    echo ""

    echo_info "Storage:"
    kubectl get pvc -n "$NAMESPACE"
    echo ""

    # Show access points via OpenShift Routes
    echo_info "OpenShift Routes:"
    kubectl get routes -n "$NAMESPACE" 2>/dev/null || echo "  No routes found"
    echo ""

    # Get route hosts for access (Centralized Gateway Architecture)
    # API route (/api) points to the gateway, UI route (/) points to UI service
    local api_route=$(kubectl get route -n "$NAMESPACE" -o jsonpath='{.items[?(@.spec.path=="/api")].spec.host}' 2>/dev/null)
    local ui_route=$(kubectl get route -n "$NAMESPACE" -l app.kubernetes.io/component=ui -o jsonpath='{.items[0].spec.host}' 2>/dev/null)

    if [ -n "$api_route" ] || [ -n "$ui_route" ]; then
        echo_info "Access Points (via OpenShift Routes):"
        if [ -n "$api_route" ]; then
            echo_info "  Gateway API Route: https://$api_route/api"
            echo_info "    All API traffic goes through the centralized Envoy gateway (JWT auth required)"
            echo_info "    - Cost Management API: https://$api_route/api/cost-management/v1/status"
            echo_info "    - Ingress API:         https://$api_route/api/ingress/ready"
            echo_info "    - ROS Recommendations: https://$api_route/api/cost-management/v1/recommendations/openshift"
        fi
        if [ -n "$ui_route" ]; then
            echo_info "  UI Route: https://$ui_route/"
        fi
        echo_info ""
        echo_info "  Note: Kruize is internal-only. Use port-forward if needed:"
        echo_info "    kubectl port-forward -n $NAMESPACE svc/${HELM_RELEASE_NAME}-kruize 8085:8085"
    else
        echo_warning "Routes not found. Use port-forwarding or check route configuration."
    fi
    echo ""

    echo_info "Useful Commands:"
    echo_info "  - View logs: kubectl logs -n $NAMESPACE -l app.kubernetes.io/instance=$HELM_RELEASE_NAME"
    echo_info "  - Delete deployment: kubectl delete namespace $NAMESPACE"
    echo_info "  - Run tests: NAMESPACE=$NAMESPACE ./run-pytest.sh"
}

# Function to check ingress controller readiness (OpenShift uses Routes, not Ingress)
check_ingress_readiness() {
    # OpenShift uses Routes managed by the router, not nginx-ingress controller
    echo_info "OpenShift Routes are managed by the built-in router - no additional ingress check needed"
    return 0
}

# Function to run health checks
run_health_checks() {
    echo_info "Running health checks..."

    local failed_checks=0

    # Test internal service connectivity first (this should always work)
    echo_info "Testing internal service connectivity..."

    # Test ROS API internally
    local api_pod=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/component=ros-api -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "$api_pod" ]; then
        if kubectl exec -n "$NAMESPACE" "$api_pod" -- curl -f -s http://localhost:8000/status >/dev/null 2>&1; then
            echo_success "✓ ROS API service is healthy (internal)"
        else
            echo_error "✗ ROS API service is not responding (internal)"
            failed_checks=$((failed_checks + 1))
        fi
    else
        echo_error "✗ ROS API pod not found"
        failed_checks=$((failed_checks + 1))
    fi

    # Test services via port-forwarding
    echo_info "Testing services via port-forwarding..."

    # Test Ingress API via port-forward
    # Note: The ingress container listens on port 8081. When JWT auth is enabled, Envoy sidecar
    # listens on port 8080 and requires authentication. For health checks, connect directly to
    # the ingress container on port 8081 to bypass JWT authentication.
    echo_info "Testing Ingress API via port-forward..."
    local ingress_pod=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/component=ingress -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [ -n "$ingress_pod" ]; then
        local ingress_pf_pid=""
        kubectl port-forward -n "$NAMESPACE" pod/"$ingress_pod" 18081:8081 --request-timeout=90s >/dev/null 2>&1 &
        ingress_pf_pid=$!
        sleep 3
        if kill -0 "$ingress_pf_pid" 2>/dev/null && curl -f -s --connect-timeout 60 --max-time 90 http://localhost:18081/ >/dev/null 2>&1; then
            echo_success "✓ Ingress API service is healthy (port-forward)"
        else
            echo_error "✗ Ingress API service is not responding (port-forward)"
            failed_checks=$((failed_checks + 1))
        fi
        # Cleanup ingress port-forward
        if [ -n "$ingress_pf_pid" ] && kill -0 "$ingress_pf_pid" 2>/dev/null; then
            kill "$ingress_pf_pid" 2>/dev/null || true
            # Wait a moment for process to terminate
            sleep 1
        fi
    else
        echo_error "✗ Ingress pod not found"
        failed_checks=$((failed_checks + 1))
    fi

    # Test Kruize API via port-forward
    echo_info "Testing Kruize API via port-forward..."
    local kruize_pf_pid=""
    kubectl port-forward -n "$NAMESPACE" svc/cost-onprem-kruize 18081:8080 --request-timeout=90s >/dev/null 2>&1 &
    kruize_pf_pid=$!
    sleep 3
    if kill -0 "$kruize_pf_pid" 2>/dev/null && curl -f -s --connect-timeout 60 --max-time 90 http://localhost:18081/listPerformanceProfiles >/dev/null 2>&1; then
        echo_success "✓ Kruize API service is healthy (port-forward)"
    else
        echo_error "✗ Kruize API service is not responding (port-forward)"
        failed_checks=$((failed_checks + 1))
    fi
    # Cleanup kruize port-forward
    if [ -n "$kruize_pf_pid" ] && kill -0 "$kruize_pf_pid" 2>/dev/null; then
        kill "$kruize_pf_pid" 2>/dev/null || true
        # Wait a moment for process to terminate
        sleep 1
    fi

    # Test external route accessibility (informational only - not counted as failure)
    echo_info "Testing external route accessibility (informational)..."
    local main_route=$(kubectl get route -n "$NAMESPACE" -o jsonpath='{.items[?(@.spec.path=="/")].spec.host}' 2>/dev/null)
    local ingress_route=$(kubectl get route -n "$NAMESPACE" -o jsonpath='{.items[?(@.spec.path=="/api/ingress")].spec.host}' 2>/dev/null)
    local kruize_route=$(kubectl get route -n "$NAMESPACE" -o jsonpath='{.items[?(@.spec.path=="/api/kruize")].spec.host}' 2>/dev/null)

    local external_accessible=0

    if [ -n "$main_route" ] && curl -f -s "http://$main_route/status" >/dev/null 2>&1; then
        echo_success "  → ROS API externally accessible: http://$main_route/status"
        external_accessible=$((external_accessible + 1))
    fi

    if [ -n "$ingress_route" ] && curl -f -s "http://$ingress_route/ready" >/dev/null 2>&1; then
        echo_success "  → Ingress API externally accessible: http://$ingress_route/ready"
        external_accessible=$((external_accessible + 1))
    fi

    if [ -n "$kruize_route" ] && curl -f -s "http://$kruize_route/api/kruize/listPerformanceProfiles" >/dev/null 2>&1; then
        echo_success "  → Kruize API externally accessible: http://$kruize_route/api/kruize/listPerformanceProfiles"
        external_accessible=$((external_accessible + 1))
    fi

    if [ $external_accessible -eq 0 ]; then
        echo_info "  → External routes not accessible (common in internal/corporate clusters)"
        echo_info "  → Use port-forwarding: kubectl port-forward svc/cost-onprem-ros-api -n $NAMESPACE 8001:8000"
    else
        echo_success "  → $external_accessible route(s) externally accessible"
    fi

    if [ $failed_checks -eq 0 ]; then
        echo_success "All core services are healthy and operational!"
    else
        echo_error "$failed_checks core service check(s) failed"
        echo_info "Check pod logs: kubectl logs -n $NAMESPACE -l app.kubernetes.io/instance=$HELM_RELEASE_NAME"
    fi

    return $failed_checks
}

# Function to cleanup
cleanup() {
    local complete_cleanup=false

    # Parse arguments
    while [ $# -gt 0 ]; do
        case "$1" in
            --complete)
                complete_cleanup=true
                ;;
            *)
                echo_warning "Unknown cleanup option: $1"
                ;;
        esac
        shift
    done

    echo_info "Cleaning up Cost Management On Premise deployment..."
    echo_info "Note: This will NOT remove AMQ Streams/Kafka. To clean them up separately:"
    echo_info "  ./deploy-kafka.sh cleanup"
    echo ""

    # Check if namespace exists
    if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
        echo_info "Namespace '$NAMESPACE' does not exist"
        return 0
    fi

    # Delete CMSC first so the operator finalizer can clean cluster-scoped resources
    local cr_name="${CR_NAME:-cost-onprem}"
    for _cmsc in "$cr_name" cost-management; do
        if kubectl get cmsc "$_cmsc" -n "$NAMESPACE" >/dev/null 2>&1; then
            echo_info "Deleting CMSC $_cmsc..."
            kubectl delete cmsc "$_cmsc" -n "$NAMESPACE" --timeout=300s --ignore-not-found || true
        fi
    done

    # Delete Helm release first (legacy chart path; no-op when only CMSC was used)
    echo_info "Deleting Helm release..."
    if helm list -n "$NAMESPACE" 2>/dev/null | grep -q "$HELM_RELEASE_NAME"; then
        helm uninstall "$HELM_RELEASE_NAME" -n "$NAMESPACE" || true
        echo_info "Waiting for Helm release deletion to complete..."
        sleep 5
    else
        echo_info "Helm release '$HELM_RELEASE_NAME' not found"
    fi

    # Delete PVCs explicitly (they often persist after namespace deletion)
    echo_info "Deleting Persistent Volume Claims..."
    local pvcs=$(kubectl get pvc -n "$NAMESPACE" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    if [ -n "$pvcs" ]; then
        for pvc in $pvcs; do
            echo_info "Deleting PVC: $pvc"
            kubectl delete pvc "$pvc" -n "$NAMESPACE" --timeout=60s || true
        done

        # Wait for PVCs to be fully deleted
        echo_info "Waiting for PVCs to be deleted..."
        local timeout=60
        local count=0
        while [ $count -lt $timeout ]; do
            local remaining_pvcs=$(kubectl get pvc -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l || echo "0")
            if [ "$remaining_pvcs" -eq 0 ]; then
                echo_success "All PVCs deleted"
                break
            fi
            echo_info "Waiting for $remaining_pvcs PVCs to be deleted... ($count/$timeout seconds)"
            sleep 2
            count=$((count + 2))
        done

        if [ $count -ge $timeout ]; then
            echo_warning "Timeout waiting for PVCs to be deleted. Some may still exist."
        fi
    else
        echo_info "No PVCs found in namespace"
    fi

    # Complete cleanup includes orphaned PVs
    if [ "$complete_cleanup" = true ]; then
        echo_info "Performing complete cleanup including orphaned Persistent Volumes..."
        local orphaned_pvs=$(kubectl get pv -o jsonpath='{.items[?(@.spec.claimRef.namespace=="'$NAMESPACE'")].metadata.name}' 2>/dev/null || true)
        if [ -n "$orphaned_pvs" ]; then
            for pv in $orphaned_pvs; do
                echo_info "Deleting orphaned PV: $pv"
                kubectl delete pv "$pv" --timeout=30s || true
            done
        else
            echo_info "No orphaned PVs found"
        fi
    fi

    # Delete namespace
    echo_info "Deleting namespace..."
    kubectl delete namespace "$NAMESPACE" --timeout=120s --ignore-not-found || true

    # Wait for namespace deletion
    echo_info "Waiting for namespace deletion to complete..."
    local timeout=120
    local count=0
    while [ $count -lt $timeout ]; do
        if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
            echo_success "Namespace deleted successfully"
            break
        fi
        echo_info "Waiting for namespace deletion... ($count/$timeout seconds)"
        sleep 2
        count=$((count + 2))
    done

    if [ $count -ge $timeout ]; then
        echo_warning "Timeout waiting for namespace deletion. It may still be terminating."
    fi

    echo_success "Cleanup completed"
}

# Function to detect RHBK (Red Hat Build of Keycloak) - OpenShift only
detect_keycloak() {
    echo_info "Detecting RHBK (Red Hat Build of Keycloak)..."

    # RHBK is only available on OpenShift clusters
    if [ "$PLATFORM" != "openshift" ]; then
        echo_info "Skipping RHBK detection - not an OpenShift cluster"
        echo_info "RHBK is only supported on OpenShift platforms"
        export KEYCLOAK_FOUND="false"
        export KEYCLOAK_NAMESPACE=""
        export KEYCLOAK_URL=""
        return 1
    fi

    local keycloak_found=false
    local keycloak_namespace=""
    local keycloak_url=""

    # Method 1: Look for RHBK Keycloak Custom Resources (k8s.keycloak.org/v2alpha1)
    echo_info "Checking for RHBK Keycloak CRs (k8s.keycloak.org/v2alpha1)..."
    if kubectl get keycloaks.k8s.keycloak.org -A >/dev/null 2>&1; then
        local keycloak_cr=$(kubectl get keycloaks.k8s.keycloak.org -A -o jsonpath='{.items[0]}' 2>/dev/null)
        if [ -n "$keycloak_cr" ]; then
            keycloak_namespace=$(echo "$keycloak_cr" | jq -r '.metadata.namespace' 2>/dev/null)
            # RHBK status.hostname is often an object {hostname, strict, ...}; fall back to string form.
            keycloak_url=$(echo "$keycloak_cr" | jq -r '
              .status.hostname.hostname // .spec.hostname.hostname //
              (select(.status.hostname|type=="string") | .status.hostname) // empty
            ' 2>/dev/null)
            keycloak_found=true
            echo_success "Found RHBK Keycloak CR in namespace: $keycloak_namespace"
            if [ -n "$keycloak_url" ]; then
                keycloak_url="https://$keycloak_url"
                echo_info "Keycloak URL: $keycloak_url"
            fi
        fi
    fi

    # Method 2: Look for common RHBK namespaces
    if [ "$keycloak_found" = false ]; then
        echo_info "Checking for RHBK namespaces..."
        for ns in keycloak keycloak-system; do
            if kubectl get namespace "$ns" >/dev/null 2>&1; then
                echo_info "Found potential RHBK namespace: $ns"
                # Check for Keycloak services in this namespace
                local keycloak_service=$(kubectl get service -n "$ns" -l "app=keycloak" -o name 2>/dev/null | head -1)
                if [ -n "$keycloak_service" ]; then
                    keycloak_namespace="$ns"
                    keycloak_found=true
                    echo_success "Confirmed RHBK service in namespace: $ns"
                    break
                fi
            fi
        done
    fi

    # Method 3: OpenShift Route detection
    if [ "$keycloak_found" = true ] && [ -z "$keycloak_url" ]; then
        echo_info "Detecting Keycloak route in OpenShift..."
        # Check for route named 'keycloak' (RHBK standard)
        keycloak_url=$(kubectl get route keycloak -n "$keycloak_namespace" -o jsonpath='{.spec.host}' 2>/dev/null)
        if [ -z "$keycloak_url" ]; then
            # Fallback to searching for any keycloak-related route
            keycloak_url=$(kubectl get route -n "$keycloak_namespace" -o jsonpath='{.items[?(@.metadata.name~="keycloak")].spec.host}' 2>/dev/null | head -1)
        fi
        if [ -n "$keycloak_url" ]; then
            keycloak_url="https://$keycloak_url"
            echo_info "Detected Keycloak route: $keycloak_url"
        fi
    fi

    # Export results for other functions
    export KEYCLOAK_FOUND="$keycloak_found"
    export KEYCLOAK_NAMESPACE="$keycloak_namespace"
    export KEYCLOAK_URL="$keycloak_url"
    export KEYCLOAK_API_VERSION="k8s.keycloak.org/v2alpha1"

    if [ "$keycloak_found" = true ]; then
        echo_success "RHBK detected successfully"
        echo_info "  API Version: k8s.keycloak.org/v2alpha1"
        echo_info "  Namespace: $keycloak_namespace"
        echo_info "  URL: ${keycloak_url:-"(auto-detect during deployment)"}"
        return 0
    else
        echo_warning "RHBK not detected in OpenShift cluster"
        echo_info "JWT authentication will be disabled"
        echo_info "To enable JWT auth, deploy RHBK using:"
        echo_info "  ./deploy-rhbk.sh"
        return 1
    fi
}

# Function to verify Keycloak client secret exists
# NOTE: Simplified - deploy-rhbk.sh now handles secret creation automatically
# This function only verifies the secret exists
verify_keycloak_client_secret() {
    local client_id="${1:-cost-management-operator}"

    if [ -z "$KEYCLOAK_NAMESPACE" ]; then
        echo_warning "Keycloak namespace not set, skipping client secret verification"
        return 1
    fi

    # Check if secret exists
    local secret_name="keycloak-client-secret-$client_id"
    if kubectl get secret "$secret_name" -n "$KEYCLOAK_NAMESPACE" >/dev/null 2>&1; then
        echo_success "✓ Client secret exists: $secret_name"
        return 0
    else
        echo_warning "Client secret not found: $secret_name"
        echo_info "  Run deploy-rhbk.sh to automatically create the client secret"
        echo_info "  The bulletproof deployment script handles secret extraction automatically"
        return 1
    fi
}

# Function to verify Keycloak UI client secret exists
verify_keycloak_ui_client_secret() {
    local client_id="${1:-cost-management-ui}"

    if [ -z "$KEYCLOAK_NAMESPACE" ]; then
        echo_warning "Keycloak namespace not set, skipping UI client secret verification"
        return 1
    fi

    # Check if secret exists
    local secret_name="keycloak-client-secret-$client_id"
    if kubectl get secret "$secret_name" -n "$KEYCLOAK_NAMESPACE" >/dev/null 2>&1; then
        echo_success "✓ UI client secret exists: $secret_name"
        return 0
    else
        echo_warning "UI client secret not found: $secret_name"
        echo_info "  Run deploy-rhbk.sh to automatically create the UI client secret"
        return 1
    fi
}

# Function to create UI secrets required by oauth2-proxy
# These are created BEFORE helm install (secrets should NEVER be in Helm charts)
create_ui_secrets() {
    echo_info "Creating UI secrets for oauth2-proxy..."

    # Operator expects {cmsc.metadata.name}-ui-oauth-client / -ui-cookie-secret
    # (sample CR name is cost-management; HELM_RELEASE_NAME is the namespace/release label).
    local release_name="${CR_NAME:-cost-onprem}"

    # 1. Create cookie secret (random session encryption key)
    local cookie_secret_name="${release_name}-ui-cookie-secret"
    if kubectl get secret "$cookie_secret_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        echo_info "Cookie secret '$cookie_secret_name' already exists"
    else
        echo_info "Creating cookie secret '$cookie_secret_name'..."
        local random_secret=$(openssl rand -base64 32 | tr -d '\n' | head -c 32)
        kubectl create secret generic "$cookie_secret_name" \
            -n "$NAMESPACE" \
            --from-literal=session-secret="$random_secret" \
            >/dev/null 2>&1
        if [ $? -eq 0 ]; then
            echo_success "✓ Created cookie secret '$cookie_secret_name'"
        else
            echo_error "Failed to create cookie secret"
            return 1
        fi
    fi

    # 2. Create OAuth client secret (from Keycloak)
    if [ -z "$KEYCLOAK_NAMESPACE" ]; then
        echo_warning "Keycloak namespace not set, skipping OAuth client secret creation"
        return 0
    fi

    local oauth_secret_name="${release_name}-ui-oauth-client"
    local keycloak_ui_secret_name="keycloak-client-secret-cost-management-ui"

    if kubectl get secret "$oauth_secret_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        echo_info "OAuth client secret '$oauth_secret_name' already exists"
        return 0
    fi

    # Check if Keycloak UI client secret exists
    if ! kubectl get secret "$keycloak_ui_secret_name" -n "$KEYCLOAK_NAMESPACE" >/dev/null 2>&1; then
        echo_warning "Keycloak UI client secret '$keycloak_ui_secret_name' not found in namespace '$KEYCLOAK_NAMESPACE'"
        echo_info "  Run deploy-rhbk.sh to create the Keycloak UI client secret first"
        return 1
    fi

    # Extract client ID and client secret from Keycloak secret
    echo_info "Extracting OAuth credentials from Keycloak..."
    local client_id=$(kubectl get secret "$keycloak_ui_secret_name" -n "$KEYCLOAK_NAMESPACE" -o jsonpath='{.data.CLIENT_ID}' 2>/dev/null | base64 -d)
    local client_secret=$(kubectl get secret "$keycloak_ui_secret_name" -n "$KEYCLOAK_NAMESPACE" -o jsonpath='{.data.CLIENT_SECRET}' 2>/dev/null | base64 -d)

    if [ -z "$client_id" ] || [ -z "$client_secret" ]; then
        echo_error "Failed to extract credentials from Keycloak secret"
        return 1
    fi

    # Create the OAuth client secret
    echo_info "Creating OAuth client secret '$oauth_secret_name'..."
    kubectl create secret generic "$oauth_secret_name" \
        -n "$NAMESPACE" \
        --from-literal=client-id="$client_id" \
        --from-literal=client-secret="$client_secret" \
        >/dev/null 2>&1

    if [ $? -eq 0 ]; then
        echo_success "✓ Created OAuth client secret '$oauth_secret_name'"
    else
        echo_error "Failed to create OAuth client secret"
        return 1
    fi
}

# Function to create Django secret for Koku
# This secret is used by Koku for Django's SECRET_KEY
create_django_secret() {
    echo_info "Creating Django secret for Koku..."

    local secret_name="cost-onprem-django-secret"

    # Check if secret already exists
    if kubectl get secret "$secret_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        echo_info "Django secret '$secret_name' already exists in namespace '$NAMESPACE'"
        return 0
    fi

    # Generate a random 50-character secret key
    local secret_key=$(openssl rand -base64 48 | tr -dc 'a-zA-Z0-9' | head -c 50)

    if [ -z "$secret_key" ]; then
        echo_error "Failed to generate Django secret key"
        return 1
    fi

    # Create the secret
    echo_info "Creating secret '$secret_name' in namespace '$NAMESPACE'..."
    kubectl create secret generic "$secret_name" \
        --from-literal=secret-key="$secret_key" \
        --namespace="$NAMESPACE"

    if [ $? -eq 0 ]; then
        echo_success "✓ Created Django secret '$secret_name'"
        return 0
    else
        echo_error "Failed to create Django secret"
        return 1
    fi
}

# Function to create Keycloak CA certificate secret for oauth2-proxy TLS trust
create_keycloak_ca_secret() {
    echo_info "Creating Keycloak CA certificate secret for TLS trust..."

    local secret_name="keycloak-ca-cert"

    if [ -z "$KEYCLOAK_NAMESPACE" ]; then
        echo_warning "Keycloak namespace not set, skipping CA certificate extraction"
        return 0
    fi

    # Check if secret already exists
    if kubectl get secret "$secret_name" -n "$NAMESPACE" >/dev/null 2>&1; then
        echo_info "Keycloak CA secret '$secret_name' already exists in namespace '$NAMESPACE'"
        return 0
    fi

    # Get Keycloak route host
    local keycloak_host=$(kubectl get route keycloak -n "$KEYCLOAK_NAMESPACE" -o jsonpath='{.spec.host}' 2>/dev/null)
    if [ -z "$keycloak_host" ]; then
        echo_warning "Could not find Keycloak route in namespace '$KEYCLOAK_NAMESPACE'"
        echo_info "  Trying alternative methods to extract CA certificate..."
    fi

    local ca_cert=""
    local temp_cert=$(mktemp)

    # Method 1: Extract from default-ingress-cert ConfigMap (most compatible)
    # This contains the full CA bundle used by OpenShift ingress
    if kubectl get configmap default-ingress-cert -n openshift-config-managed >/dev/null 2>&1; then
        echo_info "Extracting CA from default-ingress-cert ConfigMap..."
        ca_cert=$(kubectl get configmap default-ingress-cert -n openshift-config-managed -o jsonpath='{.data.ca-bundle\.crt}' 2>/dev/null)
        if [ -n "$ca_cert" ]; then
            echo_success "✓ Extracted CA from default-ingress-cert ConfigMap"
        fi
    fi

    # Method 2: Extract from OpenShift router CA secret
    if [ -z "$ca_cert" ]; then
        if kubectl get secret router-ca -n openshift-ingress-operator >/dev/null 2>&1; then
            echo_info "Extracting CA from OpenShift router-ca secret..."
            ca_cert=$(kubectl get secret router-ca -n openshift-ingress-operator -o jsonpath='{.data.tls\.crt}' | base64 -d 2>/dev/null)
            if [ -n "$ca_cert" ]; then
                echo "$ca_cert" > "$temp_cert"
                if openssl x509 -noout -text -in "$temp_cert" >/dev/null 2>&1; then
                    echo_success "✓ Extracted CA from router-ca secret"
                else
                    ca_cert=""
                fi
            fi
        fi
    fi

    # Method 3: Extract from Keycloak route directly using openssl
    if [ -z "$ca_cert" ] && [ -n "$keycloak_host" ]; then
        echo_info "Extracting CA from Keycloak route: $keycloak_host..."
        ca_cert=$(echo | openssl s_client -connect "$keycloak_host:443" -servername "$keycloak_host" 2>/dev/null | openssl x509 2>/dev/null)
        if [ -n "$ca_cert" ]; then
            echo "$ca_cert" > "$temp_cert"
            if openssl x509 -noout -text -in "$temp_cert" >/dev/null 2>&1; then
                echo_success "✓ Extracted CA from Keycloak route"
            else
                ca_cert=""
            fi
        fi
    fi

    # Cleanup temp file
    rm -f "$temp_cert"

    if [ -z "$ca_cert" ]; then
        echo_error "Failed to extract Keycloak CA certificate using any method"
        echo_info "  The oauth2-proxy may fail to connect to Keycloak with TLS errors"
        echo_info "  You can manually create the secret with:"
        echo_info "    kubectl create secret generic $secret_name --from-file=ca.crt=<your-ca-cert> -n $NAMESPACE"
        return 1
    fi

    # Create the secret
    echo_info "Creating secret '$secret_name' in namespace '$NAMESPACE'..."
    kubectl create secret generic "$secret_name" \
        --from-literal=ca.crt="$ca_cert" \
        --namespace="$NAMESPACE"

    if [ $? -eq 0 ]; then
        echo_success "✓ Created Keycloak CA secret '$secret_name'"
        return 0
    else
        echo_error "Failed to create Keycloak CA secret"
        return 1
    fi
}

# Function to setup JWT authentication
setup_jwt_authentication() {
    echo_info "Configuring JWT authentication..."

    # JWT authentication is enabled on OpenShift (requires Keycloak)
    export JWT_AUTH_ENABLED="true"
    echo_info "JWT authentication: Enabled"
    echo_info "  JWT Method: Envoy native JWT filter"
    echo_info "  Requires: RHBK (Red Hat Build of Keycloak) deployed"

    # Detect RHBK for configuration
    if detect_keycloak; then
        echo_info "  RHBK Namespace: $KEYCLOAK_NAMESPACE"
        echo_info "  RHBK API: $KEYCLOAK_API_VERSION"

        # Verify operator client secret exists (created by deploy-rhbk.sh)
        echo_info "Verifying Keycloak operator client secret exists..."
        verify_keycloak_client_secret "cost-management-operator" || \
            echo_warning "Operator client secret not found. Run ./deploy-rhbk.sh to create it."

        # Verify UI client secret exists (created by deploy-rhbk.sh)
        echo_info "Verifying Keycloak UI client secret exists..."
        verify_keycloak_ui_client_secret "cost-management-ui" || \
            echo_warning "UI client secret not found. Run ./deploy-rhbk.sh to create it."
    else
        echo_warning "RHBK not detected - ensure it's deployed before using JWT authentication"
    fi

    return 0
}

# Function to set platform-specific configurations
set_platform_config() {
    echo_info "Using OpenShift configuration"

    # Use openshift-values.yaml if no custom values file is specified
    if [ -z "$VALUES_FILE" ]; then
        local openshift_values="$SCRIPT_DIR/../openshift-values.yaml"
        if [ -f "$openshift_values" ]; then
            VALUES_FILE="$openshift_values"
            echo_info "Using OpenShift values file: $openshift_values"
        else
            echo_warning "OpenShift values file not found: $openshift_values"
            echo_info "Using base values — cluster-specific overrides will be auto-detected"
        fi
    else
        echo_info "Using custom values file: $VALUES_FILE"
    fi
}

# Main execution
main() {
    # Process additional arguments
    HELM_EXTRA_ARGS=()

    while [ $# -gt 0 ]; do
        case "$1" in
            --namespace|-n)
                # Script argument - set namespace
                NAMESPACE="$2"
                shift 2
                ;;
            --values|-f)
                # Script argument - set values file
                VALUES_FILE="$2"
                shift 2
                ;;
            --set|--set-string|--set-file|--set-json)
                # These are Helm arguments, collect them
                HELM_EXTRA_ARGS+=("$1" "$2")
                shift 2
                ;;
            --help|-h)
                echo "Usage: $0 [OPTIONS]"
                echo ""
                echo "Options:"
                echo "  --namespace, -n NAME    Set the namespace (default: cost-onprem)"
                echo "  --values, -f FILE       Specify values file"
                echo "  --set KEY=VALUE         Set Helm values"
                echo "  --help, -h              Show this help"
                exit 0
                ;;
            *)
                # Unknown argument, skip it
                echo_warning "Unknown argument: $1 (ignoring)"
                shift
                ;;
        esac
    done

    echo_info "Cost Management On Premise Helm Chart Installation"
    echo_info "=================================================="

    # Check prerequisites
    if ! check_prerequisites; then
        exit 1
    fi

    # Detect platform (OpenShift vs Kubernetes)
    detect_platform

    # Setup JWT authentication prerequisites (if applicable)
    setup_jwt_authentication

    # Set platform-specific configuration based on auto-detection
    if ! set_platform_config; then
        exit 1
    fi

    echo_info "Configuration:"
    echo_info "  Platform: $PLATFORM"
    echo_info "  Helm Release: $HELM_RELEASE_NAME"
    echo_info "  Namespace: $NAMESPACE"
    if [ -n "$VALUES_FILE" ]; then
        echo_info "  Values File: $VALUES_FILE"
    fi
    if [ "$JWT_AUTH_ENABLED" = "true" ]; then
        echo_info "  JWT Authentication: Enabled"
        echo_info "  Keycloak Namespace: $KEYCLOAK_NAMESPACE"
    else
        echo_info "  JWT Authentication: Disabled"
    fi
    echo ""

    # Create namespace
    if ! create_namespace; then
        exit 1
    fi

    # Check if the user has pre-configured S3 storage in their values file.
    # When objectStorage.endpoint is set, the user manages their own S3
    # infrastructure and the script skips all S3 auto-detection, credential
    # creation, and bucket creation.
    export USER_S3_CONFIGURED="false"
    export USER_S3_SECRET_NAME=""
    if [ -n "$VALUES_FILE" ] && [ -f "$VALUES_FILE" ] && command_exists yq; then
        local user_endpoint
        user_endpoint=$(yq '.objectStorage.endpoint // ""' "$VALUES_FILE" 2>/dev/null)
        if [ -n "$user_endpoint" ]; then
            USER_S3_CONFIGURED="true"
            USER_S3_SECRET_NAME=$(yq '.objectStorage.secretName // ""' "$VALUES_FILE" 2>/dev/null)
            echo_info "S3 storage pre-configured in values file:"
            echo_info "  Endpoint: $user_endpoint"
            echo_info "  Port: $(yq '.objectStorage.port // 443' "$VALUES_FILE" 2>/dev/null)"
            echo_info "  SSL: $(yq '.objectStorage.useSSL // true' "$VALUES_FILE" 2>/dev/null)"
            if [ -n "$USER_S3_SECRET_NAME" ]; then
                echo_info "  Credentials Secret: $USER_S3_SECRET_NAME (user-managed)"
            else
                echo_info "  Credentials Secret: will be created by install script (or use S3_ACCESS_KEY/S3_SECRET_KEY)"
            fi
            echo_info "Helm S3 --set overrides skipped (values file defines object storage)"
        fi
    fi

    # Detect external ObjectBucketClaim (OBC) for Direct Ceph RGW deployments
    # This must happen BEFORE creating storage credentials
    # Skip if user has already configured S3 in values.yaml
    export USING_EXTERNAL_OBC="false"
    if [ "$USER_S3_CONFIGURED" = "false" ] && [ "$PLATFORM" = "openshift" ]; then
        if detect_external_obc "ros-data-ceph" "$NAMESPACE"; then
            USING_EXTERNAL_OBC="true"
            echo_info "Direct Ceph RGW deployment detected via external OBC"
            echo_info "  Storage credentials and bucket creation will be skipped"
        fi
    fi

    # Determine whether to skip storage credential and bucket creation:
    #   - USER_S3_CONFIGURED=true + secretName set → skip credentials + buckets (fully user-managed)
    #   - USER_S3_CONFIGURED=true + no secretName + S3_ACCESS_KEY set → create credentials + buckets
    #   - USER_S3_CONFIGURED=true + no secretName + no env creds → create credentials, skip buckets
    #   - USING_EXTERNAL_OBC=true                  → skip credentials + buckets (OBC provides both)
    #   - Otherwise                                → auto-detect and create both
    local skip_storage_credentials="false"
    local skip_bucket_creation="false"

    if [ "$USER_S3_CONFIGURED" = "true" ]; then
        if [ -n "$USER_S3_SECRET_NAME" ]; then
            skip_storage_credentials="true"
            skip_bucket_creation="true"
        elif [ -n "${S3_ACCESS_KEY:-}" ] && [ -n "${S3_SECRET_KEY:-}" ]; then
            skip_bucket_creation="false"
        else
            skip_bucket_creation="true"
        fi
    elif [ "$USING_EXTERNAL_OBC" = "true" ]; then
        skip_storage_credentials="true"
        skip_bucket_creation="true"
    fi

    echo ""
    echo_info "════════════════════════════════════════════════════════════"
    echo_info "  Creating Secrets (Before Helm Install)"
    echo_info "════════════════════════════════════════════════════════════"
    echo ""

    # Create database credentials secret (skip when using external database)
    local database_deploy
    database_deploy=$(get_helm_value "database.deploy" "true")
    if [ "$database_deploy" = "false" ]; then
        echo_info "Skipping database credentials creation (database.deploy=false, using external database)"
        echo_info "Ensure the database credentials secret already exists in namespace '$NAMESPACE'"
    else
        if ! create_database_credentials_secret; then
            echo_error "Failed to create database credentials. Cannot proceed with installation."
            exit 1
        fi
    fi

    # Create storage credentials secret
    # Track the secret name so we can tell Helm about it via --set objectStorage.secretName
    # This prevents Helm from trying to create a conflicting placeholder secret.
    export STORAGE_CREDENTIALS_SECRET=""
    if [ "$skip_storage_credentials" = "false" ]; then
        if ! create_storage_credentials_secret; then
            echo_error "Failed to create storage credentials. Cannot proceed with installation."
            exit 1
        fi
        STORAGE_CREDENTIALS_SECRET="$(storage_credentials_secret_name)"
    else
        if [ -n "$USER_S3_SECRET_NAME" ]; then
            echo_info "Skipping storage credentials creation (using secret: $USER_S3_SECRET_NAME)"
        else
            echo_info "Skipping storage credentials creation (using OBC credentials)"
        fi
    fi

    echo ""
    echo_success "✓ All required secrets created successfully"
    echo ""

    # Create S3 buckets
    if [ "$skip_bucket_creation" = "false" ]; then
        if ! create_s3_buckets; then
            echo_warning "Failed to create S3 buckets. Data storage may not work correctly."
        fi
    else
        if [ "$USER_S3_CONFIGURED" = "true" ]; then
            echo_info "Skipping bucket creation (user-managed S3 storage)"
        else
            echo_info "Skipping bucket creation (bucket provided by external OBC)"
        fi
    fi

    # Create UI secrets (cookie + OAuth client) - required before helm install
    if [ "$JWT_AUTH_ENABLED" = "true" ] && [ -n "$KEYCLOAK_NAMESPACE" ]; then
        if ! create_ui_secrets; then
            echo_warning "Failed to create UI secrets. UI OAuth may not work correctly."
            echo_info "  Ensure deploy-rhbk.sh has been run to create the Keycloak UI client secret"
        fi
    fi

    # Create Keycloak CA certificate secret for oauth2-proxy TLS trust (if Keycloak is available)
    if [ "$JWT_AUTH_ENABLED" = "true" ] && [ -n "$KEYCLOAK_NAMESPACE" ]; then
        if ! create_keycloak_ca_secret; then
            echo_warning "Failed to create Keycloak CA secret. oauth2-proxy may have TLS issues."
            echo_info "  You may need to manually create the keycloak-ca-cert secret"
        fi
    fi

    # Create Django secret for Koku (if costManagement is enabled)
    if ! create_django_secret; then
        echo_warning "Failed to create Django secret. Koku may not start correctly."
    fi

    # Verify AMQ Streams operator and Kafka cluster are available
    if ! verify_kafka; then
        echo_error "AMQ Streams/Kafka prerequisites not met"
        exit 1
    fi

    # Pre-flight validation (advisory — warns about missing cluster-specific values)
    preflight_validate

    # Deploy Helm chart
    if ! deploy_helm_chart; then
        exit 1
    fi

    # Wait for pods to be ready
    if ! wait_for_pods; then
        echo_warning "Some pods may not be ready. Continuing..."
    fi

    # Trigger initial Keycloak-to-RBAC sync (if enabled) before health checks
    trigger_keycloak_sync

    # Show deployment status
    show_status

    # Check ingress readiness before health checks
    check_ingress_readiness

    # Run health checks
    echo_info "Waiting 30 seconds for services to stabilize before running health checks..."

    # Show pod status before health checks
    echo_info "Pod status before health checks:"
    kubectl get pods -n "$NAMESPACE" -o wide

    if ! run_health_checks; then
        echo_warning "Some health checks failed, but deployment completed successfully"
        echo_info "Services may need more time to be fully ready"
        echo_info "You can run health checks manually later or check pod logs for issues"
        echo_info "Pod logs: kubectl logs -n $NAMESPACE -l app.kubernetes.io/instance=$HELM_RELEASE_NAME"
    fi

    echo ""
    echo_success "Cost Management On Prem Helm chart installation completed!"
    echo_info "The services are now running in namespace '$NAMESPACE'"
    echo_info "Next: Run NAMESPACE=$NAMESPACE ./run-pytest.sh to test the deployment"
}

# Allow sourcing for unit tests: only run main when executed directly.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then

# Handle script arguments
case "${1:-}" in
    "cleanup")
        shift  # Remove "cleanup" from arguments
        cleanup "$@"
        exit 0
        ;;
    "status")
        detect_platform
        show_status
        exit 0
        ;;
    "health")
        detect_platform
        run_health_checks
        exit $?
        ;;
    "help"|"-h"|"--help")
        echo "Usage: $0 [command] [options] [--set key=value ...]"
        echo ""
        echo "Platform Detection:"
        echo "  The script automatically detects whether you're running on:"
        echo "  - OpenShift (production configuration: HA, large resources, ODF storage)"
        echo "  - Kubernetes (development configuration: single node, small resources)"
        echo ""
        echo "Prerequisites:"
        echo "  Before running this installation, ensure you have:"
        echo "  1. AMQ Streams operator and Kafka cluster deployed (run ./deploy-kafka.sh)"
        echo "     OR provide KAFKA_BOOTSTRAP_SERVERS for existing Kafka"
        echo "  2. For OpenShift with JWT auth: RHBK (optional, run ./deploy-rhbk.sh)"
        echo ""
        echo "Commands:"
        echo "  (none)              - Install Cost Management On Premise Helm chart"
        echo "  cleanup             - Delete Helm release and namespace (preserves PVs)"
        echo "  cleanup --complete  - Complete removal including Persistent Volumes"
        echo "                        Note: AMQ Streams/Kafka are NOT removed. Use ./deploy-kafka.sh cleanup"
        echo "  status              - Show deployment status"
        echo "  health              - Run health checks"
        echo "  help                - Show this help message"
        echo ""
        echo "Helm Arguments:"
        echo "  --set key=value     - Set individual values (can be used multiple times)"
        echo "  --set-string key=value - Set string values"
        echo "  --set-file key=path - Set values from file"
        echo "  --set-json key=json - Set JSON values"
        echo ""
        echo "Uninstall/Reinstall Workflow:"
        echo "  # For clean reinstall with fresh data:"
        echo "  $0 cleanup --complete    # Remove everything including data volumes"
        echo "  ./deploy-kafka.sh cleanup  # Optional: remove AMQ Streams/Kafka too"
        echo "  ./deploy-kafka.sh      # Optional: reinstall AMQ Streams/Kafka"
        echo "  $0                       # Fresh installation"
        echo ""
        echo "  # For reinstall preserving data:"
        echo "  $0 cleanup               # Remove workloads but keep volumes"
        echo "  $0                       # Reinstall (reuses existing volumes and Kafka)"
        echo ""
        echo "Environment Variables:"
        echo "  HELM_RELEASE_NAME       - Name of Helm release (default: cost-onprem)"
        echo "  NAMESPACE               - Kubernetes namespace (default: cost-onprem)"
        echo "  VALUES_FILE             - Path to custom values file (optional)"
        echo "  USE_LOCAL_CHART         - Use local chart instead of Helm repository (default: false)"
        echo "  LOCAL_CHART_PATH        - Path to local chart directory (default: ../cost-onprem)"
        echo "  CHART_VERSION           - Pin a specific chart version (default: latest)"
        echo "  KAFKA_BOOTSTRAP_SERVERS - Bootstrap servers for existing Kafka (skips verification)"
        echo "                            Example: my-kafka-bootstrap.kafka:9092"
        echo ""
        echo "S3 Storage Configuration:"
        echo "  Option 1: values.yaml (ODF, AWS S3, or any S3-compatible backend)"
        echo "    Set objectStorage.endpoint, port, useSSL, objectStorage.s3.region (AWS),"
        echo "    and optionally objectStorage.secretName if you manage credentials yourself."
        echo "    When endpoint is set, the script skips Helm --set for S3 (your file wins)."
        echo "    Credentials/buckets are still created unless secretName is set (then fully user-managed)."
        echo ""
        echo "  Option 2: Environment variables (typical for AWS S3)"
        echo "    S3_ENDPOINT           - Hostname only (e.g., s3.us-east-1.amazonaws.com)"
        echo "    S3_REGION             - Region for SigV4 (e.g., us-east-1); sets objectStorage.s3.region"
        echo "    S3_PORT               - S3 port (default: 443)"
        echo "    S3_USE_SSL            - Whether to use TLS (default: true)"
        echo "    S3_VERIFY_SSL         - Verify TLS (default: false; use true for AWS/public CAs)"
        echo "    S3_ACCESS_KEY / S3_SECRET_KEY - Create storage secret when both are set"
        echo "    S3_BUCKET_PREFIX      - Unique prefix for AWS buckets (<p>-ingress-upload, <p>-koku, <p>-ros)"
        echo "    S3_BUCKET_INGRESS / S3_BUCKET_KOKU / S3_BUCKET_ROS - Override individual bucket names"
        echo ""
        echo "  Option 3 (Automated): In-cluster backends"
        echo "    OBC auto-detection    - ObjectBucketClaim 'ros-data-ceph'"
        echo "    NooBaa / ODF          - When NooBaa CR exists in openshift-storage"
        echo ""
        echo "  Option 4: Overrides"
        echo "    SKIP_S3_SETUP         - Skip S3 bucket creation (default: false)"
        echo ""
        echo "Chart Source Options:"
        echo "  - Default: Installs from Helm chart repository (recommended)"
        echo "  - Pinned: Set CHART_VERSION={VERSION} to pin a specific chart version"
        echo "  - Local: Set USE_LOCAL_CHART=true to use local chart directory"
        echo "  - Chart Path: Set LOCAL_CHART_PATH to specify custom chart location"
        echo "  - Examples:"
        echo "    $0                                                     # Install latest from Helm repo"
        echo "    CHART_VERSION=0.2.9 $0                                 # Install specific version"
        echo "    USE_LOCAL_CHART=true LOCAL_CHART_PATH=../cost-onprem $0"
        echo ""
        echo "Examples:"
        echo "  # Complete fresh installation"
        echo "  ./deploy-kafka.sh                           # Install AMQ Streams and Kafka first"
        echo "  USE_LOCAL_CHART=true LOCAL_CHART_PATH=../cost-onprem $0  # Then install Cost Management On Premise"
        echo ""
        echo "  # Install from Helm repository (with AMQ Streams already deployed)"
        echo "  ./deploy-kafka.sh                           # Install prerequisites"
        echo "  $0                                            # Install Cost Management On Premise from Helm repo"
        echo ""
        echo "  # Custom namespace and release name"
        echo "  NAMESPACE=my-namespace HELM_RELEASE_NAME=my-release \\"
        echo "    USE_LOCAL_CHART=true LOCAL_CHART_PATH=../cost-onprem $0"
        echo ""
        echo "  # Use existing Kafka on cluster (deployed by other means)"
        echo "  KAFKA_BOOTSTRAP_SERVERS=my-kafka-bootstrap.my-namespace:9092 $0"
        echo ""
        echo "  # With custom overrides"
        echo "  USE_LOCAL_CHART=true LOCAL_CHART_PATH=../cost-onprem $0 \\"
        echo "    --set database.ros.storage.size=200Gi"
        echo ""
        echo "  # Install latest from Helm repository"
        echo "  $0"
        echo ""
        echo "  # Cleanup and reinstall"
        echo "  $0 cleanup --complete && USE_LOCAL_CHART=true LOCAL_CHART_PATH=../cost-onprem $0"
        echo ""
        echo "Platform Detection:"
        echo "  - Automatically detects Kubernetes vs OpenShift"
        echo "  - Uses openshift-values.yaml for OpenShift if available"
        echo "  - Auto-detects optimal storage class for platform"
        echo "  - Verifies AMQ Streams operator and Kafka cluster prerequisites"
        echo ""
        echo "Deployment Scenarios:"
        echo "  1. Fresh deployment (recommended):"
        echo "     ./deploy-kafka.sh    # Deploy AMQ Streams and Kafka first"
        echo "     $0                     # Deploy Cost Management On Premise"
        echo "     - Auto-detects platform (OpenShift or Kubernetes)"
        echo "     - Verifies AMQ Streams/Kafka prerequisites"
        echo "     - Deploys Cost Management On Premise with platform-specific configuration"
        echo ""
        echo "  2. With existing Kafka (external):"
        echo "     KAFKA_BOOTSTRAP_SERVERS=kafka.example.com:9092 $0"
        echo "     - Uses provided Kafka bootstrap servers"
        echo "     - Skips AMQ Streams/Kafka verification"
        echo ""
        echo "  3. Custom configuration:"
        echo "     ./deploy-kafka.sh"
        echo "     $0 --set key=value"
        echo "     - Override any Helm value"
        echo "     - Platform detection still applies"
        echo ""
        echo "Requirements:"
        echo "  - kubectl must be configured with target cluster"
        echo "  - helm must be installed"
        echo "  - jq must be installed for JSON processing"
        echo "  - Target cluster must have sufficient resources"
        exit 0
        ;;
esac

# Run main function
main "$@"

fi  # end BASH_SOURCE guard
