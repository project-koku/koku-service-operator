#!/usr/bin/env bash
# Unit tests for safe_curl in scripts/deploy-rhbk.sh (no cluster required).
#
# Prow e2e-pytest failed in assign_sync_client_realm_roles: curl exit 6
# (Could not resolve host) under `set -e` aborted the script with no log
# after the section header.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Stub curl is expected to fail; keep retry warnings off the test-scripts log.
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
printf '{"access_token":"renewed"}'
exit 0
EOF
chmod +x "${stub_dir}/curl"
export SAFE_CURL_RETRIES=5

token=""
status=0
set +e
token="$(_obtain_admin_token https://keycloak.example admin secret)"
status=$?
set -e
assert_eq "$status" "0" "_obtain_admin_token is set -e safe after DNS retries"
assert_eq "$token" "renewed" "_obtain_admin_token returns token after two DNS failures"
assert_eq "$(cat "$attempts_file")" "3" "_obtain_admin_token retried via safe_curl"

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

token=""
status=0
set +e
token="$(_obtain_admin_token https://keycloak.example admin secret)"
status=$?
set -e
assert_eq "$status" "0" "_obtain_admin_token returns 0 when DNS never recovers"
assert_eq "$token" "" "_obtain_admin_token is empty after exhausted retries"

export KEYCLOAK_ADMIN_BASE="http://127.0.0.1:18080"
assert_eq "$(keycloak_admin_base)" "http://127.0.0.1:18080" \
  "keycloak_admin_base honors KEYCLOAK_ADMIN_BASE (port-forward)"
unset KEYCLOAK_ADMIN_BASE

# --- ensure_keycloak_admin_base self-healing (Prow port-forward) ------------
# CI: oc port-forward to keycloak-service died after the Keycloak pod restarted
# (realm import). ensure_keycloak_admin_base early-returned on the cached-but-
# dead http://127.0.0.1:18080, so every later admin phase hit curl exit 7 and
# auth failed. Fix: probe liveness and rebuild a dead tunnel instead of reusing
# it. These tests stub the oc/curl collaborators (no cluster required).

# _keycloak_pf_alive reports dead when no port-forward has been started.
_KEYCLOAK_PF_PID=""
status=0
set +e
_keycloak_pf_alive
status=$?
set -e
assert_eq "$status" "1" "_keycloak_pf_alive reports dead when no port-forward PID cached"

# ensure_keycloak_admin_base rebuilds a dead tunnel rather than reusing it.
# A backgrounded oc stub keeps a real PID alive so cleanup has something to kill;
# the curl stub makes the health probe pass immediately. Proof of rebuild is the
# refreshed base (stale :9999 -> :18080), which is race-free unlike the async oc.
oc_marker="${stub_dir}/oc-portforward-invoked"
rm -f "$oc_marker"
cat >"${stub_dir}/oc" <<EOF
#!/usr/bin/env bash
touch "$oc_marker"
exec sleep 300
EOF
chmod +x "${stub_dir}/oc"
cat >"${stub_dir}/curl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${stub_dir}/curl"

_keycloak_pf_alive() { return 1; }                    # simulate a dead tunnel
export OPENSHIFT_CI=true
export KEYCLOAK_ADMIN_BASE="http://127.0.0.1:9999"    # stale cached target
ensure_keycloak_admin_base
assert_eq "$KEYCLOAK_ADMIN_BASE" "http://127.0.0.1:18080" \
  "ensure_keycloak_admin_base rebuilds a dead tunnel and refreshes the cached base"
cleanup_keycloak_admin_proxy                          # kill the backgrounded stub oc
trap 'rm -rf "$stub_dir"' EXIT                        # ensure_* clobbered the EXIT trap

# ensure_keycloak_admin_base reuses a live tunnel without a new port-forward.
rm -f "$oc_marker"
_keycloak_pf_alive() { return 0; }                    # simulate a healthy tunnel
export KEYCLOAK_ADMIN_BASE="http://127.0.0.1:18080"
ensure_keycloak_admin_base
assert_eq "$([ -e "$oc_marker" ] && echo yes || echo no)" "no" \
  "ensure_keycloak_admin_base reuses a live tunnel (no new oc port-forward)"
assert_eq "$KEYCLOAK_ADMIN_BASE" "http://127.0.0.1:18080" \
  "ensure_keycloak_admin_base keeps the cached base when the tunnel is alive"

unset KEYCLOAK_ADMIN_BASE OPENSHIFT_CI
rm -f "${stub_dir}/oc"

rhbk_default="$(grep -E '^RHBK_SCRIPT=' "${ROOT}/scripts/deploy-byoi.sh" || true)"
assert_eq "$rhbk_default" \
  'RHBK_SCRIPT="${RHBK_SCRIPT:-${ROOT}/scripts/deploy-rhbk.sh}"' \
  "deploy-byoi.sh defaults RHBK_SCRIPT to this repo"

# --- review #24: credentials never appear in curl's argv (behavior test) ---
arglog="${stub_dir}/argv.log"
filelog="${stub_dir}/files.log"
cat >"${stub_dir}/curl" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${arglog}"
# Show what the @file indirection actually delivers:
for a in "\$@"; do
  if [[ "\$a" == @* && -f "\${a#@}" ]]; then
    printf 'FILE %s: ' "\$a" >> "${filelog}"
    cat "\${a#@}" >> "${filelog}"
    printf '\n' >> "${filelog}"
  fi
done
printf '{"ok":true}'
exit 0
EOF
chmod +x "${stub_dir}/curl"
: >"$arglog"
: >"$filelog"

out="$(safe_curl -X POST https://keycloak.example/token \
  -H "Authorization: Bearer super-secret-token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "username=admin" -d "password=super-secret-password" -d "grant_type=password")"
assert_eq "$out" '{"ok":true}' "hardened safe_curl still returns the body"
argv_dump="$(cat "$arglog")"
files_dump="$(cat "$filelog")"
assert_not_contains() {
  local hay="$1" needle="$2" msg="$3"
  if [[ "$hay" == *"$needle"* ]]; then
    echo "FAIL: ${msg} (found '${needle}')" >&2
    fail=1
  fi
}
assert_not_contains "$argv_dump" "super-secret-token" "bearer token not in argv"
assert_not_contains "$argv_dump" "super-secret-password" "password not in argv"
assert_not_contains "$argv_dump" "Bearer" "no Authorization value in argv at all"
case "$argv_dump" in
  *"--header @/"*) : ;;
  *) echo "FAIL: Authorization header passed via @file (got: $argv_dump)" >&2; fail=1 ;;
esac
case "$argv_dump" in
  *"--data @/"*) : ;;
  *) echo "FAIL: body passed via @file (got: $argv_dump)" >&2; fail=1 ;;
esac
# The @files must carry the real values (still sent, just not in argv):
case "$files_dump" in
  *"Authorization: Bearer super-secret-token"*) : ;;
  *) echo "FAIL: header file carries the Authorization value" >&2; fail=1 ;;
esac
case "$files_dump" in
  *"username=admin&password=super-secret-password&grant_type=password"*) : ;;
  *) echo "FAIL: body file joins repeated --data with &" >&2; fail=1 ;;
esac
case "$argv_dump" in
  *"Content-Type: application/x-www-form-urlencoded"*) : ;;
  *) echo "FAIL: non-secret headers pass through" >&2; fail=1 ;;
esac

# TLS: --cacert when the CA is set; -k otherwise
: >"$arglog"
ca_file="${stub_dir}/ca.pem"
printf 'CERT\n' >"$ca_file"
RHBK_CURL_CA="$ca_file" safe_curl https://keycloak.example/realms/master >/dev/null
case "$(cat "$arglog")" in
  *"--cacert ${ca_file}"*) : ;;
  *) echo "FAIL: router CA is used for TLS verification" >&2; fail=1 ;;
esac
case "$(cat "$arglog")" in
  *" -k "*) echo "FAIL: -k must not combine with --cacert" >&2; fail=1 ;;
esac
: >"$arglog"
RHBK_CURL_CA="" safe_curl https://keycloak.example/realms/master >/dev/null
case "$(cat "$arglog")" in
  *" -k "*) : ;;
  *) echo "FAIL: without a CA the call falls back to -k" >&2; fail=1 ;;
esac

# TLS verification failure is actionable (cross-model review #5)
cat >"${stub_dir}/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl: (60) SSL certificate problem" >&2
exit 60
EOF
chmod +x "${stub_dir}/curl"
tls_out="$(safe_curl https://keycloak.example/realms/master 2>&1)" && tls_rc=0 || tls_rc=$?
assert_eq "$tls_rc" "0" "TLS failure returns safely under set -e"
case "$tls_out" in
  *"RHBK_INSECURE_TLS=1"*) : ;;
  *) echo "FAIL: TLS failure must point at RHBK_INSECURE_TLS=1 (got: $tls_out)" >&2; fail=1 ;;
esac
case "$tls_out" in
  *"TLS verification failed"*) : ;;
  *) echo "FAIL: TLS failure is named as such" >&2; fail=1 ;;
esac

if [[ "$fail" -ne 0 ]]; then
  echo "deploy-rhbk_test: FAILED" >&2
  exit 1
fi
echo "deploy-rhbk_test: ok"
