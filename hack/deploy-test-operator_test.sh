#!/usr/bin/env bash
# No-cluster tests for hack/deploy-test-operator.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${ROOT}/hack/deploy-test-operator.sh"
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

assert_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "FAIL: ${msg}" >&2
    echo "  missing: ${needle}" >&2
    fail=1
  fi
}

assert_exit() {
  local code="$1" msg="$2"
  shift 2
  set +e
  "$@" >/dev/null 2>&1
  local rc=$?
  set -e
  assert_eq "$rc" "$code" "$msg (exit $rc)"
}

# --- help ---
assert_exit 0 "help exits 0" "$SCRIPT" --help

# --- IMG required for full deploy ---
assert_exit 1 "missing IMG rejected" "$SCRIPT" --dry-run --deploy-s4

# --- tests-only without IMG ---
out="$("$SCRIPT" --tests-only --dry-run --no-ui 2>&1)"
assert_contains "$out" "tests only:" "tests-only in plan"
assert_contains "$out" "DRY RUN" "dry-run banner"

# --- dry-run full plan with IMG ---
out="$(IMG=quay.io/example/op:test "$SCRIPT" --dry-run --deploy-s4 --verbose 2>&1)"
assert_contains "$out" "Deploying Red Hat Build of Keycloak" "plans RHBK"
assert_contains "$out" "Deploying AMQ Streams" "plans Kafka"
assert_contains "$out" "Deploying S4" "plans S4"
assert_contains "$out" "deploy-incluster.sh" "plans operator"
assert_contains "$out" "Applying CostManagementServiceConfig" "plans CMSC"
assert_contains "$out" "pytest" "plans pytest"

# --- dry-run ODF path: service CA for objectStorage TLS ---
out="$(IMG=quay.io/example/op:test "$SCRIPT" --dry-run --verbose 2>&1)"
assert_contains "$out" "ODF S3 CA" "plan shows ODF service CA"
assert_contains "$out" "OpenShift service CA" "dry-run would ensure service CA secret"

# --- parse duration helper ---
# shellcheck disable=SC1091
source "${ROOT}/hack/lib/deploy-test-operator.bash"
assert_eq "$(dto_parse_duration_seconds 45m)" "2700" "45m → seconds"
assert_eq "$(dto_parse_duration_seconds 90s)" "90" "90s → seconds"
warn_file="$(mktemp)"
assert_eq "$(dto_parse_duration_seconds 1h 2>"$warn_file")" "2700" "1h → default seconds"
assert_contains "$(cat "$warn_file")" "unrecognized" "1h emits duration warning"
rm -f "$warn_file"
warn_file="$(mktemp)"
assert_eq "$(dto_parse_duration_seconds bogus 2>"$warn_file")" "2700" "bogus → default seconds"
assert_contains "$(cat "$warn_file")" "unrecognized" "bogus emits duration warning"
rm -f "$warn_file"

# --- additive: must not invoke chart-only entrypoints ---
lib="${ROOT}/hack/lib/deploy-test-operator.bash"
for pattern in 'deploy-test-cost-onprem' 'install-cmsc' 'helm install' 'helm upgrade'; do
  if grep -E "execute_script.*${pattern}|\\$\\{ROOT\\}/scripts/${pattern}" "$lib" 2>/dev/null; then
    echo "FAIL: lib must not invoke chart path: ${pattern}" >&2
    fail=1
  fi
done
if grep -q 'scripts/deploy-test-cost-onprem.sh' "$lib"; then
  echo "FAIL: lib must not call deploy-test-cost-onprem.sh" >&2
  fail=1
fi

if [[ "$fail" -ne 0 ]]; then
  exit 1
fi
echo "OK: deploy-test-operator tests passed"
