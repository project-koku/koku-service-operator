#!/usr/bin/env bash
# No-cluster checks for the OLM pre-prod demo entrypoint.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

help="$(cd "$ROOT" && ./scripts/demo-preprod-olm.sh --help)"
case "$help" in
  *"published catalog"*) ;;
  *)
    echo "FAIL: help must describe the console catalog phase" >&2
    exit 1
    ;;
esac
case "$help" in
  *"./scripts/demo-preprod-olm.sh --watch"*) ;;
  *)
    echo "FAIL: help must describe the watch phase" >&2
    exit 1
    ;;
esac
case "$help" in
  *"./scripts/demo-preprod-olm.sh --console"*) ;;
  *)
    echo "FAIL: help must describe the Console URL mode" >&2
    exit 1
    ;;
esac

plan="$(cd "$ROOT" && ./scripts/demo-preprod-olm.sh --dry-run)"
case "$plan" in
  *"quay.io/project-koku/koku-service-operator-catalog:latest"*) ;;
  *)
    echo "FAIL: dry run must use the published catalog" >&2
    exit 1
    ;;
esac
case "$plan" in
  *"operator:   v0.0.1"*) ;;
  *)
    echo "FAIL: dry run must pin the expected operator version" >&2
    exit 1
    ;;
esac
case "$plan" in
  *"Add the catalog and install the operator in the OpenShift console"*) ;;
  *)
    echo "FAIL: dry run must retain the console OLM step" >&2
    exit 1
    ;;
esac

if grep -Fq 'CONSOLE_URL="https://console-openshift-console.${DOMAIN}"' "$ROOT/scripts/demo-preprod-olm.sh"; then
  :
else
  echo "FAIL: prepare output must derive the OpenShift Console URL" >&2
  exit 1
fi
if grep -Fq 'if [[ "$MODE" == "console" ]]' "$ROOT/scripts/demo-preprod-olm.sh"; then
  :
else
  echo "FAIL: helper must offer a no-mutation Console URL mode" >&2
  exit 1
fi
if grep -Fq 'Console username: ${CONSOLE_USERNAME}' "$ROOT/scripts/demo-preprod-olm.sh" && \
   grep -Fq 'retrieve it from the ClusterBot provisioning output' "$ROOT/scripts/demo-preprod-olm.sh"; then
  :
else
  echo "FAIL: helper must accurately describe the ClusterBot Console credentials" >&2
  exit 1
fi

echo "demo-preprod-olm tests OK"
