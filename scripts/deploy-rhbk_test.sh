#!/usr/bin/env bash
# Unit tests for safe_curl in scripts/deploy-rhbk.sh (no cluster required).
#
# Prow e2e-pytest failed in assign_sync_client_realm_roles: curl exit 6
# (Could not resolve host) under `set -e` aborted the script with no log
# after the section header.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Stub curl is expected to fail; keep retry warnings off the test-hack log.
export LOG_LEVEL=ERROR
# shellcheck disable=SC1091
source "${ROOT}/scripts/deploy-rhbk.sh"

fail=0
assert_eq() {
  local got="$1" want="$2" msg="$3"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL: ${msg}" >&2
    echo "  got:  ${got}" >&2
    echo "  want: ${want}" >&2
    fail=1
  fi
}

stub_dir="$(mktemp -d "${TMPDIR:-/tmp}/safe-curl-test.XXXXXX")"
trap 'rm -rf "$stub_dir"' EXIT
attempts_file="${stub_dir}/attempts"
echo 0 >"$attempts_file"

cat >"${stub_dir}/curl" <<EOF
#!/usr/bin/env bash
n=\$(cat "$attempts_file")
n=\$((n + 1))
echo "\$n" >"$attempts_file"
if [[ "\$n" -lt 3 ]]; then
  echo "curl: (6) Could not resolve host: keycloak.example" >&2
  exit 6
fi
printf '{"access_token":"ok"}'
exit 0
EOF
chmod +x "${stub_dir}/curl"

export PATH="${stub_dir}:${PATH}"
export SAFE_CURL_RETRIES=5
export SAFE_CURL_RETRY_DELAY=0

body=""
status=0
set +e
body="$(safe_curl -X POST https://keycloak.example/token)"
status=$?
set -e

assert_eq "$status" "0" "safe_curl returns 0 after retries (set -e safe)"
assert_eq "$body" '{"access_token":"ok"}' "safe_curl returns body after two DNS failures"
assert_eq "$(cat "$attempts_file")" "3" "safe_curl retried curl exit 6 twice then succeeded"

echo 0 >"$attempts_file"
cat >"${stub_dir}/curl" <<EOF
#!/usr/bin/env bash
n=\$(cat "$attempts_file")
n=\$((n + 1))
echo "\$n" >"$attempts_file"
echo "curl: (6) Could not resolve host" >&2
exit 6
EOF
chmod +x "${stub_dir}/curl"

export SAFE_CURL_RETRIES=3
body=""
status=0
set +e
body="$(safe_curl https://keycloak.example/token)"
status=$?
set -e

assert_eq "$status" "0" "safe_curl returns 0 after exhausted retries"
assert_eq "$body" "" "safe_curl body is empty after exhausted retries"
assert_eq "$(cat "$attempts_file")" "3" "safe_curl used all retries"

rhbk_default="$(grep -E '^RHBK_SCRIPT=' "${ROOT}/hack/deploy-byoi.sh" || true)"
assert_eq "$rhbk_default" \
  'RHBK_SCRIPT="${RHBK_SCRIPT:-${ROOT}/scripts/deploy-rhbk.sh}"' \
  "deploy-byoi.sh defaults RHBK_SCRIPT to this repo"

if [[ "$fail" -ne 0 ]]; then
  echo "deploy-rhbk_test: FAILED" >&2
  exit 1
fi
echo "deploy-rhbk_test: ok"
