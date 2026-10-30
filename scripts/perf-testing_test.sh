#!/usr/bin/env bash
# No-cluster tests for apply_perf_profile_config() in scripts/lib/perf-testing.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
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
    echo "  unexpected: ${needle}" >&2
    fail=1
  fi
}

# Run apply_perf_profile_config in a subshell with stubbed logging helpers.
run_profile() {
  local profile="$1"
  (
    if [[ "$profile" == "__unset__" ]]; then unset PERF_PROFILE; fi
    log_info() { echo "$*"; }
    log_step() { echo "$*"; }
    log_success() { echo "$*"; }
    log_warning() { echo "$*"; }
    log_error() { echo "$*"; }
    log_verbose() { :; }
    export NAMESPACE=test-ns CMSC_NAME=test-cmsc DRY_RUN=true
    if [[ "$profile" != "__unset__" ]]; then export PERF_PROFILE="$profile"; fi
    # shellcheck source=lib/perf-testing.sh
    source "${ROOT}/scripts/lib/perf-testing.sh"
    apply_perf_profile_config
  ) 2>&1 || true
}

# Extract the -p JSON from the dry-run oc patch line.
patch_json() {
  sed -n "s/.*--type merge -p '\\(.*\\)'.*/\\1/p" <<<"$1" | head -1
}

out="$(run_profile large)"
assert_contains "$out" "oc patch cmsc test-cmsc -n test-ns --type merge" "large: single oc patch cmsc"
assert_contains "$out" '"profile":"large"' "large: patch sets profile large"
assert_not_contains "$out" "oc scale" "large: no oc scale"
assert_contains "$out" '"maxUploadSize":null' "large: nulls ingress maxUploadSize"
assert_contains "$out" '"ocp":{"replicas":null,"resources":null}' "large: nulls ocp worker overrides"
assert_not_contains "$out" '"kruize":{"replicas"' "large: kruize replicas not nulled"
assert_contains "$out" "listener=3 ocp=3 summary=3 ros-processor=3 kruize=1" "large: expected replicas"

pj="$(patch_json "$out")"
if ! jq -e '.spec.profile == "large"
  and (.spec.kruize | has("replicas") | not)
  and .spec.kruize.resources == null
  and (.spec.ingress | has("maxUploadSize") and .maxUploadSize == null)
  and (.spec.costManagement.celery.workers.ocp | has("replicas") and .replicas == null)' <<<"$pj" >/dev/null; then
  echo "FAIL: large: patch JSON invalid or wrong shape: ${pj}" >&2; fail=1
fi
assert_contains "$out" "would wait up to 600s for CMSC status.observedGeneration" "large: wait step announced"
assert_not_contains "$out" "Progressing" "large: wait step does not depend on Progressing"

out="$(run_profile __unset__)"
assert_contains "$out" '"profile":"small"' "unset PERF_PROFILE maps to small"

out="$(run_profile bogus)"
assert_contains "$out" "Unknown PERF_PROFILE 'bogus'" "unknown profile fails fast"
assert_not_contains "$out" "oc patch" "unknown profile does not patch"

out="$(run_profile baseline)"
assert_contains "$out" '"profile":"small"' "baseline maps to small"
assert_contains "$out" "listener=2 ocp=2 summary=2 ros-processor=1 kruize=1" "small: expected replicas"

out="$(run_profile medium)"
assert_contains "$out" '"profile":"medium"' "medium: patch sets profile"
assert_contains "$out" "listener=2 ocp=2 summary=2 ros-processor=2 kruize=1" "medium: expected replicas"

out="$(run_profile xlarge)"
assert_contains "$out" '"profile":"xlarge"' "xlarge: patch sets profile"
assert_contains "$out" "listener=3 ocp=3 summary=3 ros-processor=3 kruize=1" "xlarge: expected replicas"

if [[ $fail -ne 0 ]]; then
  echo "perf-testing_test.sh: FAILED" >&2
  exit 1
fi
echo "perf-testing_test.sh: OK"
