#!/usr/bin/env bash
#
# Mirror the Keycloak UI confidential-client credentials into the CR namespace
# as the Secret oauth2-proxy expects (keys: client-id, client-secret).
#
# The operator does NOT create Keycloak or this Secret. Keycloak/RHBK is always
# BYOI (same as Kafka). The operator only validates the Secret via
# spec.ui.oauthClientSecretRef (default: {cr-name}-ui-oauth-client) and sets
# Condition UIReady. Cookie session secret is operator-managed separately.
#
# Chart equivalent: install-helm-chart.sh create_oauth_client_secret (copies
# keycloak-client-secret-cost-management-ui CLIENT_ID/CLIENT_SECRET →
# {release}-ui-oauth-client client-id/client-secret).
#
# Prerequisites:
#   - Keycloak/RHBK deployed (e.g. scripts/deploy-rhbk.sh)
#   - Secret keycloak-client-secret-cost-management-ui in KEYCLOAK_NAMESPACE
#
# Usage (from operator repo root):
#   ./config/samples/byoi/mirror-ui-oauth-secret.sh
#   CR_NAME=cost-onprem NAMESPACE=cost-tests ./config/samples/byoi/mirror-ui-oauth-secret.sh
#   ./config/samples/byoi/mirror-ui-oauth-secret.sh --force   # replace existing target
#
set -euo pipefail

KEYCLOAK_NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
KEYCLOAK_UI_SECRET="${KEYCLOAK_UI_SECRET:-keycloak-client-secret-cost-management-ui}"
NAMESPACE="${NAMESPACE:-cost-byoi}"
CR_NAME="${CR_NAME:-cost-management}"
TARGET_SECRET="${TARGET_SECRET:-${CR_NAME}-ui-oauth-client}"
FORCE=0

for arg in "$@"; do
  case "$arg" in
    --force|-f) FORCE=1 ;;
    -h|--help)
      sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "Unknown argument: $arg" >&2
      exit 1
      ;;
  esac
done

if ! kubectl get secret "$KEYCLOAK_UI_SECRET" -n "$KEYCLOAK_NAMESPACE" >/dev/null 2>&1; then
  echo "error: Secret ${KEYCLOAK_UI_SECRET} not found in namespace ${KEYCLOAK_NAMESPACE}" >&2
  echo "  Deploy Keycloak first (scripts/deploy-rhbk.sh)." >&2
  exit 1
fi

client_id=$(kubectl get secret "$KEYCLOAK_UI_SECRET" -n "$KEYCLOAK_NAMESPACE" \
  -o jsonpath='{.data.CLIENT_ID}' | base64 -d)
client_secret=$(kubectl get secret "$KEYCLOAK_UI_SECRET" -n "$KEYCLOAK_NAMESPACE" \
  -o jsonpath='{.data.CLIENT_SECRET}' | base64 -d)

if [[ -z "$client_id" || -z "$client_secret" ]]; then
  echo "error: ${KEYCLOAK_UI_SECRET} missing CLIENT_ID or CLIENT_SECRET" >&2
  exit 1
fi

if kubectl get secret "$TARGET_SECRET" -n "$NAMESPACE" >/dev/null 2>&1; then
  if [[ "$FORCE" -eq 0 ]]; then
    echo "Secret ${NAMESPACE}/${TARGET_SECRET} already exists (use --force to replace)"
    exit 0
  fi
  kubectl delete secret "$TARGET_SECRET" -n "$NAMESPACE"
fi

kubectl create secret generic "$TARGET_SECRET" -n "$NAMESPACE" \
  --from-literal=client-id="$client_id" \
  --from-literal=client-secret="$client_secret"

echo "Mirrored ${KEYCLOAK_NAMESPACE}/${KEYCLOAK_UI_SECRET} → ${NAMESPACE}/${TARGET_SECRET}"
echo "  Point spec.ui.oauthClientSecretRef.name at this Secret if not using the default name."
