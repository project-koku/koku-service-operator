#!/usr/bin/env bash
# No-cluster tests for hack/ci CMSC issuer injection (Prow e2e-pytest 401s).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SAMPLE="${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml"
INJECT="${ROOT}/hack/ci/inject_cmsc_issuer.py"

fail=0
assert_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "FAIL: ${msg}" >&2
    echo "  missing: ${needle}" >&2
    fail=1
  fi
}

assert_not_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "FAIL: ${msg}" >&2
    echo "  unexpectedly present: ${needle}" >&2
    fail=1
  fi
}

tmp="$(mktemp)"
cp "$SAMPLE" "$tmp"
python3 "$INJECT" "$tmp" "https://keycloak-keycloak.apps.ci-op-example.ci.aws.devcluster.openshift.com"
cr="$(cat "$tmp")"
assert_contains "$cr" 'url: "http://keycloak-service.keycloak.svc.cluster.local:8080"' \
  "JWKS url stays in-cluster"
assert_contains "$cr" \
  'issuerURL: "https://keycloak-keycloak.apps.ci-op-example.ci.aws.devcluster.openshift.com"' \
  "issuerURL set to public Route"
assert_not_contains "$cr" '# issuerURL:' "commented placeholder removed"
assert_contains "$cr" "insecureSkipVerify: true" "oauth-proxy skip-verify for claimed-cluster router cert"
assert_not_contains "$cr" "#   insecureSkipVerify" "commented skip-verify removed"

# Idempotent overwrite of a stale issuer.
python3 "$INJECT" "$tmp" "https://keycloak-keycloak.apps.other.example.com"
cr="$(cat "$tmp")"
assert_contains "$cr" 'issuerURL: "https://keycloak-keycloak.apps.other.example.com"' \
  "re-inject replaces issuerURL"
assert_not_contains "$cr" "ci-op-example" "old issuer gone"
rm -f "$tmp"

if ! python3 "$INJECT" /dev/null "http://insecure.example.com" 2>/dev/null; then
  :
else
  echo "FAIL: http issuer must be rejected (CEL requires https)" >&2
  fail=1
fi

if [[ "$fail" -ne 0 ]]; then
  echo "hack/ci/e2e_test.sh: FAILED" >&2
  exit 1
fi
echo "hack/ci/e2e_test.sh: ok"
