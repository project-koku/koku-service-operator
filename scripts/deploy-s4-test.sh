#!/bin/bash

# Deploy S4 (Super Simple Storage Service) for Testing in OpenShift Cluster
# This script deploys an S4 instance for testing the cost-onprem chart
# with S4 instead of cluster object storage. This simulates the CI environment in an OCP cluster.
# S4 is an open-source Ceph RGW with SQLite backend (S3-compatible).

set -e  # Exit on any error

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

echo_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

echo_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

echo_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Configuration
NAMESPACE=${1:-"s4-test"}
ACTION=${2:-"deploy"}
S4_RELEASE_TAG="${S4_RELEASE_TAG:-v0.2.2}"
S4_REPO="${S4_REPO:-rh-aiservices-bu/s4}"
STORAGE_SIZE=${STORAGE_SIZE:-"10Gi"}

# Handle cleanup subcommand
if [ "$ACTION" = "cleanup" ]; then
    echo ""
    echo "════════════════════════════════════════════════════════════"
    echo "  Cleanup S4 Resources"
    echo "════════════════════════════════════════════════════════════"
    echo ""
    echo "Namespace: $NAMESPACE"
    echo ""
    echo_info "Removing S4 resources from namespace: $NAMESPACE"
    helm uninstall s4 -n "$NAMESPACE" 2>/dev/null || true
    kubectl delete secret s4-credentials -n "$NAMESPACE" --ignore-not-found
    echo_success "S4 cleanup complete in namespace: $NAMESPACE"
    exit 0
fi

echo ""
echo "════════════════════════════════════════════════════════════"
echo "  Deploy S4 for Testing"
echo "════════════════════════════════════════════════════════════"
echo ""
echo "Namespace:      $NAMESPACE"
echo "S4 Release:     $S4_REPO @ $S4_RELEASE_TAG"
echo "Storage Size:   $STORAGE_SIZE"
echo ""

# Check prerequisites
for cmd in kubectl helm curl; do
    if ! command -v "$cmd" &> /dev/null; then
        echo_error "$cmd not found. Please install $cmd."
        exit 1
    fi
done

if ! command -v oc &> /dev/null; then
    echo_warning "oc not found. Using kubectl (some features may not work on OpenShift)"
fi

# Check cluster connectivity
if ! kubectl get nodes >/dev/null 2>&1; then
    echo_error "Cannot connect to cluster. Please check your kubectl configuration."
    exit 1
fi

# Detect platform
if kubectl get routes.route.openshift.io >/dev/null 2>&1; then
    PLATFORM="openshift"
    echo_success "Detected OpenShift platform"
else
    PLATFORM="kubernetes"
    echo_success "Detected Kubernetes platform"
fi

# Create namespace
echo_info "Creating namespace: $NAMESPACE"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
echo_success "Namespace ready"

# Generate S4 credentials
echo_info "Generating S4 credentials..."
S4_ACCESS_KEY="s4admin"
S4_SECRET_KEY=$(openssl rand -base64 32 | tr -d "=+/" | cut -c1-32)

# Download S4 release archive
S4_TMPDIR=$(mktemp -d)
trap 'rm -rf "$S4_TMPDIR"' EXIT

echo_info "Downloading S4 release $S4_RELEASE_TAG from $S4_REPO..."
TARBALL_URL="https://github.com/$S4_REPO/archive/refs/tags/$S4_RELEASE_TAG.tar.gz"
if ! curl -fsSL "$TARBALL_URL" | tar xz -C "$S4_TMPDIR" --strip-components=1; then
    echo_error "Failed to download S4 release $S4_RELEASE_TAG from $S4_REPO"
    exit 1
fi
echo_success "S4 release $S4_RELEASE_TAG downloaded"

# Verify chart exists
if [ ! -d "$S4_TMPDIR/charts/s4" ]; then
    echo_error "S4 Helm chart not found at charts/s4 in the downloaded release"
    exit 1
fi

# Deploy S4 via Helm
echo_info "Deploying S4 via Helm..."
if ! helm upgrade --install s4 "$S4_TMPDIR/charts/s4" \
    --namespace "$NAMESPACE" \
    --set s3.accessKeyId="$S4_ACCESS_KEY" \
    --set s3.secretAccessKey="$S4_SECRET_KEY" \
    --set auth.enabled=false \
    --set route.enabled=false \
    --set storage.data.size="$STORAGE_SIZE" \
    ${STORAGE_CLASS:+--set storage.data.storageClassName="$STORAGE_CLASS"} \
    --wait --timeout 300s; then
    echo_error "Failed to deploy S4"
    echo_info "Check pod status: kubectl get pods -n $NAMESPACE"
    echo_info "Check pod logs: kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=s4"
    exit 1
fi
echo_success "S4 Helm chart installed"

# Create s4-credentials secret (cost-onprem format: access-key / secret-key)
echo_info "Creating S4 credentials secret..."
kubectl create secret generic s4-credentials \
    --namespace="$NAMESPACE" \
    --from-literal=access-key="$S4_ACCESS_KEY" \
    --from-literal=secret-key="$S4_SECRET_KEY" \
    --dry-run=client -o yaml | kubectl apply -f -
echo_success "Credentials secret created"

# Wait for S4 pod to be ready
echo_info "Waiting for S4 to be ready..."
if kubectl wait --for=condition=ready pod \
    -l app.kubernetes.io/name=s4 \
    -n "$NAMESPACE" \
    --timeout=300s; then
    echo_success "S4 is ready"
else
    echo_error "S4 failed to become ready within 5 minutes"
    echo_info "Check pod status: kubectl get pods -n $NAMESPACE"
    echo_info "Check pod logs: kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=s4"
    exit 1
fi

# Get connection details
echo ""
echo "════════════════════════════════════════════════════════════"
echo "  S4 Deployment Complete"
echo "════════════════════════════════════════════════════════════"
echo ""

# Internal endpoint
INTERNAL_ENDPOINT="s4.$NAMESPACE.svc.cluster.local"
echo_success "S4 S3 Endpoint (internal): $INTERNAL_ENDPOINT:7480"

# Credentials
echo ""
echo_info "Credentials:"
echo "  Access Key: $S4_ACCESS_KEY"
echo "  Secret Key: $S4_SECRET_KEY"
echo ""
echo_info "To retrieve credentials later:"
echo "  kubectl get secret s4-credentials -n $NAMESPACE -o jsonpath='{.data.access-key}' | base64 -d"
echo "  kubectl get secret s4-credentials -n $NAMESPACE -o jsonpath='{.data.secret-key}' | base64 -d"
echo ""

# Usage instructions
echo "════════════════════════════════════════════════════════════"
echo "  Usage Instructions"
echo "════════════════════════════════════════════════════════════"
echo ""
echo "To deploy cost-onprem with this S4 instance:"
echo ""
echo "  S3_ENDPOINT=\"$INTERNAL_ENDPOINT\" S3_PORT=7480 S3_USE_SSL=false \\"
echo "    ./scripts/install-cmsc.sh"
echo ""
echo "Or set the environment variables first:"
echo ""
echo "  export S3_ENDPOINT=\"$INTERNAL_ENDPOINT\""
echo "  export S3_PORT=7480"
echo "  export S3_USE_SSL=false"
echo "  ./scripts/install-cmsc.sh"
echo ""
echo "To delete this S4 deployment:"
echo "  $0 $NAMESPACE cleanup"
echo ""
