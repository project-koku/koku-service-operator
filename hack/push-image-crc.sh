#!/usr/bin/env bash
# Push a local container image to the CRC/OpenShift internal registry.
#
# The registry Route often fails TLS verification from Docker/Podman on macOS.
# This script uses a temporary port-forward and skopeo with --dest-tls-verify=false.
#
# Usage:
#   ./hack/push-image-crc.sh <full-image-ref>
#
# Example:
#   make docker-build IMG=default-route-openshift-image-registry.apps-crc.testing/cost-onprem/koku-service-operator:dev
#   ./hack/push-image-crc.sh default-route-openshift-image-registry.apps-crc.testing/cost-onprem/koku-service-operator:dev
#
# Prerequisites:
#   - CRC running; oc logged in
#   - skopeo installed (brew install skopeo)
#   - image present in local Docker (Rancher Desktop) or passable via docker-archive
set -euo pipefail

IMG="${1:?Usage: $0 <registry-host/namespace/image:tag>}"
PORT="${CRC_REGISTRY_PORT:-5001}"
TAR="${CRC_IMAGE_TAR:-/tmp/crc-image-push.tar}"

export KUBECONFIG="${KUBECONFIG:-${HOME}/.crc/machines/crc/kubeconfig}"

if ! command -v skopeo >/dev/null 2>&1; then
  echo "skopeo is required (brew install skopeo)" >&2
  exit 1
fi

if [[ "$IMG" != */*/*:* ]]; then
  echo "Expected image ref like <host>/<namespace>/<name>:<tag>, got: $IMG" >&2
  exit 1
fi

REPO_TAG="${IMG#*/}"  # namespace/name:tag

echo "=== Push to CRC registry via localhost:${PORT} ==="
echo "Image:  $IMG"
echo "Target: localhost:${PORT}/${REPO_TAG}"
echo ""

oc port-forward -n openshift-image-registry "svc/image-registry" "${PORT}:5000" >/dev/null &
PF_PID=$!
cleanup() {
  kill "${PF_PID}" 2>/dev/null || true
}
trap cleanup EXIT

# Wait for port-forward
for _ in $(seq 1 30); do
  if curl -sf -o /dev/null "http://localhost:${PORT}/v2/" 2>/dev/null; then
    break
  fi
  sleep 1
done

if command -v docker >/dev/null 2>&1 && docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "Saving from Docker..."
  docker save "$IMG" -o "$TAR"
elif command -v podman >/dev/null 2>&1 && podman image exists "$IMG" 2>/dev/null; then
  echo "Saving from Podman..."
  podman save "$IMG" -o "$TAR"
elif [[ -f "$TAR" ]]; then
  echo "Using existing archive: $TAR"
else
  echo "Image not found locally and no archive at $TAR" >&2
  exit 1
fi

skopeo copy \
  --dest-creds "$(oc whoami):$(oc whoami -t)" \
  --dest-tls-verify=false \
  "docker-archive:${TAR}" \
  "docker://localhost:${PORT}/${REPO_TAG}"

echo ""
echo "Pushed. Cluster pull ref:"
echo "  image-registry.openshift-image-registry.svc:5000/${REPO_TAG}"
echo "  ${IMG}"
