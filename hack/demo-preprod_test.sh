#!/usr/bin/env bash
# Unit tests for hack/lib/demo-preprod.bash (no cluster required).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "${ROOT}/hack/lib/demo-preprod.bash"

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

assert_not_contains() {
  local haystack="$1" needle="$2" msg="$3"
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "FAIL: ${msg}" >&2
    echo "  unexpectedly present: ${needle}" >&2
    fail=1
  fi
}

# --- Dockerfile: OpenShift non-root UID cannot write WORKDIR /workspace ---
tmp="$(mktemp)"
cat >"$tmp" <<'EOF'
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o wait-for ./cmd/wait-for/
COPY --from=builder /workspace/manager /manager
COPY --from=builder /workspace/wait-for /wait-for
EOF
patch_operator_dockerfile "$tmp"
patched="$(cat "$tmp")"
assert_contains "$patched" "go build -a -o /tmp/manager cmd/main.go" "build manager to /tmp"
assert_contains "$patched" "go build -a -o /tmp/wait-for ./cmd/wait-for/" "build wait-for to /tmp"
assert_contains "$patched" "COPY --from=builder /tmp/manager /manager" "copy manager from /tmp"
assert_contains "$patched" "COPY --from=builder /tmp/wait-for /wait-for" "copy wait-for from /tmp"
assert_not_contains "$patched" "-o manager cmd/main.go" "no WORKDIR-relative manager output"
rm -f "$tmp"

# --- CR: replace placeholder domain, set public issuer + skip-verify ---
tmp="$(mktemp)"
render_preprod_cr \
  "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" \
  "$tmp" \
  "apps.example.test" \
  "https://keycloak-keycloak.apps.example.test"
cr="$(cat "$tmp")"
assert_contains "$cr" 'clusterDomain: "apps.example.test"' "clusterDomain patched"
assert_not_contains "$cr" "apps.cluster.example.com" "placeholder domain gone"
assert_contains "$cr" 'issuerURL: "https://keycloak-keycloak.apps.example.test"' "issuerURL set"
assert_contains "$cr" "insecureSkipVerify: true" "skip-verify for lab router CA"
assert_contains "$cr" $'  ros:\n    enabled: false' "ROS stays off (sample CR, not a demo env var)"
rm -f "$tmp"

# --- Chart root: worktree ROOT is not the sibling of cost-onprem-chart ---
got="$(default_chart_root "/tmp/koku-service-operator/.worktrees/cost-7688-gaps" "/tmp/koku-service-operator/.git")"
assert_eq "$got" "/tmp/cost-onprem-chart" "chart sits next to the main git dir, not the worktree"

# --- Preflight must surface the real API error (expired Cluster Bot, etc.) ---
stub_dir="$(mktemp -d)"
cat >"${stub_dir}/kubectl-dead" <<'EOF'
#!/bin/bash
echo "Unable to connect to the server: dial tcp: lookup api.expired.example: no such host" >&2
exit 1
EOF
cat >"${stub_dir}/kubectl-empty-domain" <<'EOF'
#!/bin/bash
exit 0
EOF
cat >"${stub_dir}/kubectl-ok" <<'EOF'
#!/bin/bash
if [[ "$1" == "whoami" ]]; then
  echo "kube:admin"
  exit 0
fi
echo "apps.chat-bot.example.com"
exit 0
EOF
chmod +x "${stub_dir}"/kubectl-*

dead_out="$(require_reachable_cluster "${stub_dir}/kubectl-dead" 2>&1)" && dead_rc=0 || dead_rc=$?
assert_eq "$dead_rc" "1" "dead cluster: require_reachable_cluster fails"
assert_contains "$dead_out" "kube API is not reachable" "dead cluster: high-level message"
assert_contains "$dead_out" "no such host" "dead cluster: original oc/kubectl error"

dead_dom="$(read_apps_domain "${stub_dir}/kubectl-dead" 2>&1)" && dead_dom_rc=0 || dead_dom_rc=$?
assert_eq "$dead_dom_rc" "1" "dead cluster: read_apps_domain fails"
assert_contains "$dead_dom" "failed to get ingress.config.openshift.io/cluster" "domain fetch: high-level message"
assert_contains "$dead_dom" "no such host" "domain fetch: original error, not swallowed"
assert_not_contains "$dead_dom" "could not read OpenShift apps domain" "no generic swallowed-domain message"

empty_out="$(read_apps_domain "${stub_dir}/kubectl-empty-domain" 2>&1)" && empty_rc=0 || empty_rc=$?
assert_eq "$empty_rc" "1" "empty spec.domain fails"
assert_contains "$empty_out" "empty spec.domain" "empty domain is explicit"

ok_dom="$(read_apps_domain "${stub_dir}/kubectl-ok" 2>&1)" && ok_rc=0 || ok_rc=$?
assert_eq "$ok_rc" "0" "live cluster: read_apps_domain succeeds"
assert_eq "$ok_dom" "apps.chat-bot.example.com" "live cluster: domain value"
rm -rf "$stub_dir"

# --help must work when invoked from hack/ (relative $0 after cd to repo root)
help_out="$(cd "${ROOT}/hack" && ./demo-preprod.sh --help 2>&1)" || {
  echo "FAIL: --help from hack/ exited $? " >&2
  echo "$help_out" >&2
  fail=1
}
assert_contains "$help_out" "./hack/demo-preprod.sh --dry-run" "--help from hack/ prints usage"
assert_not_contains "$help_out" "No such file or directory" "--help from hack/ must not sed a relative \$0"

if [[ "$fail" -ne 0 ]]; then
  echo "demo-preprod tests FAILED" >&2
  exit 1
fi
echo "demo-preprod tests OK"
