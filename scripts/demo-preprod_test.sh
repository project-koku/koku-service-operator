#!/usr/bin/env bash
# Unit tests for scripts/lib/demo-preprod.bash (no cluster required).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "${ROOT}/scripts/lib/demo-preprod.bash"

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

# Exercise the shipped Dockerfile too: a stale fixture must not hide changes to
# build flags, package paths, or commands that inspect the resulting binaries.
tmp="$(mktemp)"
cp "${ROOT}/Dockerfile" "$tmp"
patch_operator_dockerfile "$tmp"
patched="$(cat "$tmp")"
assert_contains "$patched" "-tags=no_openssl,nativefips -o /tmp/manager ./cmd" "preserve manager FIPS tags and package build"
assert_contains "$patched" "-tags=no_openssl,nativefips -o /tmp/wait-for ./cmd/wait-for/" "preserve helper FIPS tags and package build"
assert_contains "$patched" "RUN go version -m /tmp/manager /tmp/wait-for" "inspect redirected binaries"
assert_contains "$patched" "COPY --from=builder /tmp/manager /manager" "copy current manager output"
assert_contains "$patched" "COPY --from=builder /tmp/wait-for /wait-for" "copy current helper output"
assert_not_contains "$patched" " -o manager " "current manager output is not WORKDIR-relative"
assert_not_contains "$patched" " -o wait-for " "current helper output is not WORKDIR-relative"
patch_operator_dockerfile "$tmp"
assert_eq "$(cat "$tmp")" "$patched" "Dockerfile patch is idempotent"
rm -f "$tmp"

# --- CR: replace placeholder domain, set public issuer + skip-verify ---
tmp="$(mktemp)"
render_preprod_cr \
  "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" \
  "$tmp" \
  "apps.example.test" \
  "https://keycloak-keycloak.apps.example.test" \
  "catalog-demo" \
  "catalog-cost"
cr="$(cat "$tmp")"
assert_contains "$cr" $'  name: catalog-cost\n  namespace: catalog-demo' "CMSC name and namespace patched"
assert_contains "$cr" 'clusterDomain: "apps.example.test"' "clusterDomain patched"
assert_not_contains "$cr" "apps.cluster.example.com" "placeholder domain gone"
assert_contains "$cr" 'issuerURL: "https://keycloak-keycloak.apps.example.test"' "issuerURL set"
assert_contains "$cr" "insecureSkipVerify: true" "skip-verify for lab router CA"
assert_contains "$cr" $'  ros:\n    enabled: false' "ROS stays off (sample CR, not a demo env var)"
rm -f "$tmp"

# --- Demo uses the in-repo Keycloak provisioner by default ---
demo_root="$(mktemp -d)"
mkdir -p "${demo_root}/scripts/lib"
cp -p "${ROOT}/scripts/demo-preprod.sh" "${ROOT}/scripts/demo-preprod.env.example" "${demo_root}/scripts/"
cp -p "${ROOT}/scripts/lib/demo-preprod.bash" "${demo_root}/scripts/lib/"
plan="$(RHBK_SCRIPT= "${demo_root}/scripts/demo-preprod.sh" --dry-run)"
assert_contains "$plan" "RHBK script: ${demo_root}/scripts/deploy-rhbk.sh" "demo defaults to in-repo RHBK script"
assert_not_contains "$plan" "CHART_ROOT" "demo does not require a chart checkout"
rm -rf "$demo_root"
plan="$(RHBK_SCRIPT=/tmp/custom-deploy-rhbk.sh ./scripts/demo-preprod.sh --dry-run)"
assert_contains "$plan" "RHBK script: /tmp/custom-deploy-rhbk.sh" "demo accepts an RHBK script override"

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

# --help must work when invoked from scripts/ (relative $0 after cd to repo root)
help_out="$(cd "${ROOT}/scripts" && ./demo-preprod.sh --help 2>&1)" || {
  echo "FAIL: --help from scripts/ exited $? " >&2
  echo "$help_out" >&2
  fail=1
}
assert_contains "$help_out" "./scripts/demo-preprod.sh --dry-run" "--help from scripts/ prints usage"
assert_not_contains "$help_out" "No such file or directory" "--help from scripts/ must not sed a relative \$0"

# --- Task 1 (#1): --source selector and current-context default ---
help_out="$(cd "$ROOT" && ./scripts/demo-preprod.sh --help 2>&1)"
assert_contains "$help_out" "--source=registry|local|manual" "help documents --source"

plan="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run 2>&1)"
assert_contains "$plan" "source:      registry" "dry run defaults to the registry source"
assert_not_contains "$plan" "clusterbot" "dry run must not default KUBE_CONTEXT to clusterbot"

plan_local="$(cd "$ROOT" && env -u KUBE_CONTEXT SOURCE=local ./scripts/demo-preprod.sh --dry-run 2>&1)"
assert_contains "$plan_local" "source:      local" "SOURCE=local selects the local source"

bad_src="$(cd "$ROOT" && env -u KUBE_CONTEXT SOURCE=bogus ./scripts/demo-preprod.sh --dry-run 2>&1)" && bad_rc=0 || bad_rc=$?
assert_eq "$bad_rc" "2" "invalid SOURCE exits 2"
assert_contains "$bad_src" "registry, local or manual" "invalid SOURCE names the valid values"

# --- Task 2 (#1): manual source, --watch/--console; demo-preprod-olm.sh absorbed ---
help_out="$(cd "$ROOT" && ./scripts/demo-preprod.sh --help 2>&1)"
assert_contains "$help_out" "--watch" "help documents --watch"
assert_contains "$help_out" "--console" "help documents --console"

if [[ -e "${ROOT}/scripts/demo-preprod-olm.sh" ]]; then
  echo "FAIL: demo-preprod-olm.sh must be absorbed into demo-preprod.sh" >&2
  fail=1
fi

fx_manual="$(mktemp)"
printf 'main\nlatest\n474d937\n' >"$fx_manual"
plan_manual="$(cd "$ROOT" && env -u KUBE_CONTEXT REGISTRY_TAG_FIXTURE="$fx_manual" SOURCE=manual ./scripts/demo-preprod.sh --dry-run 2>&1)"
rm -f "$fx_manual"
assert_contains "$plan_manual" "CR file:     " "manual plan shows the CR file"
assert_contains "$plan_manual" "Install the operator through the OpenShift console" "manual plan previews the console step"
assert_contains "$plan_manual" "quay.io/project-koku/koku-service-operator-catalog:latest" "manual plan falls back to latest when no semver tag exists"
assert_contains "$plan_manual" "operator:    v0.0.1" "manual plan names the operator version"
assert_contains "$plan_manual" "--watch" "manual plan points at --watch"

# Ported from demo-preprod-olm_test.sh (offline greps against the merged script)
grep -Fq 'CONSOLE_URL="https://console-openshift-console.${DOMAIN}"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: prepare output must derive the OpenShift Console URL" >&2
  fail=1
}
grep -Fq 'if [[ "$DEMO_CONSOLE" == "1" ]]' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: helper must offer a no-mutation Console URL mode" >&2
  fail=1
}
grep -Fq 'Console username: ${CONSOLE_USERNAME}' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: helper must print the console username" >&2
  fail=1
}
grep -Fq "crc console --credentials" "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: helper must explain where the kubeadmin password comes from" >&2
  fail=1
}

# --- Task 3 (#2): version discovery and selection ---
fx="$(mktemp)"
printf 'main\nlatest\n474d937\nv0.0.1\nv0.0.3\nv0.0.2\n' >"$fx"

out="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog 2>&1 >/dev/null)"
assert_eq "$out" "" "no warning when a semver tag exists"

printf 'main\nlatest\n474d937\n' >"$fx"
out="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog "")"
assert_eq "$out" "latest" "no semver falls back to latest"
warn="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog 2>&1 >/dev/null)"
assert_contains "$warn" "no semver" "fallback sets a loud warning"
assert_contains "$warn" "moving target" "warning explains the consequence"

out="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog 474d937)"
assert_eq "$out" "474d937" "commit SHA passthrough"

printf 'v0.0.1\nmain\n474d937\n' >"$fx"
out="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog v0.0.1@sha256:abc123)"
assert_eq "$out" "v0.0.1@sha256:abc123" "digest suffix preserved"

out="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog 474d9370123456789abcdef0123456789abcdef0)"
assert_eq "$out" "474d937" "full SHA normalized to the short tag"

out="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog 0.0.1)"
assert_eq "$out" "v0.0.1" "semver without v normalized"

bad_ver="$(REGISTRY_TAG_FIXTURE="$fx" resolve_catalog_version project-koku/koku-service-operator-catalog v9.9.9 2>&1)" && bad_rc=0 || bad_rc=$?
assert_eq "$bad_rc" "1" "unknown version fails"
assert_contains "$bad_ver" "not found" "unknown version error names the problem"
assert_contains "$bad_ver" "v0.0.1" "unknown version error lists available tags"
rm -f "$fx"

out="$(REGISTRY_TAG_FIXTURE="/nonexistent-tag-fixture-$$" resolve_catalog_version project-koku/koku-service-operator-catalog v0.0.1 2>/dev/null)" && unreach_rc=0 || unreach_rc=$?
assert_eq "$unreach_rc" "0" "unreachable registry passes an explicit hint through"
assert_eq "$out" "v0.0.1" "unreachable registry uses the hint as-is"
warn="$(REGISTRY_TAG_FIXTURE="/nonexistent-tag-fixture-$$" resolve_catalog_version project-koku/koku-service-operator-catalog v0.0.1 2>&1 >/dev/null)"
assert_contains "$warn" "could not verify" "unreachable registry warns"

fx="$(mktemp)"
printf 'main\nlatest\n474d937\nv0.0.1\nv0.0.3\n' >"$fx"
plan_v="$(cd "$ROOT" && env -u KUBE_CONTEXT REGISTRY_TAG_FIXTURE="$fx" ./scripts/demo-preprod.sh --dry-run 2>&1)"
assert_contains "$plan_v" "catalog:     quay.io/project-koku/koku-service-operator-catalog:v0.0.3" "dry run prints the resolved catalog image"
plan_pin="$(cd "$ROOT" && env -u KUBE_CONTEXT REGISTRY_TAG_FIXTURE="$fx" VERSION=v0.0.1 ./scripts/demo-preprod.sh --dry-run 2>&1)"
assert_contains "$plan_pin" "catalog:     quay.io/project-koku/koku-service-operator-catalog:v0.0.1" "VERSION pins the catalog image"
rm -f "$fx"

# --- Task 4 (#3): OLM manifests and install path ---
mans="$(olm_manifests "quay.io/example/catalog:v0.0.3" "beta" "cost-demo" "koku-service-operator-catalog")"
assert_contains "$mans" "kind: CatalogSource" "manifests include the CatalogSource"
assert_contains "$mans" "name: koku-service-operator-catalog" "CatalogSource named"
assert_contains "$mans" "namespace: openshift-marketplace" "CatalogSource in openshift-marketplace"
assert_contains "$mans" "sourceType: grpc" "CatalogSource is grpc"
assert_contains "$mans" "image: quay.io/example/catalog:v0.0.3" "CatalogSource pins the resolved image"
assert_contains "$mans" "kind: OperatorGroup" "manifests include the OperatorGroup"
assert_not_contains "$mans" "targetNamespaces" "OperatorGroup is AllNamespaces (no targetNamespaces)"
assert_contains "$mans" "kind: Subscription" "manifests include the Subscription"
assert_contains "$mans" "installPlanApproval: Automatic" "Subscription is Automatic"
assert_contains "$mans" "channel: beta" "Subscription uses the channel"
assert_contains "$mans" "namespace: cost-demo" "OG and Subscription live in the app namespace"
assert_contains "$mans" "source: koku-service-operator-catalog" "Subscription points at the CatalogSource"

warn_out="$(warn_existing_install cost-demo 2>&1)"
assert_contains "$warn_out" "--reset" "existing-install warning names --reset"
assert_contains "$warn_out" "0.0.1" "existing-install warning explains the equal-version trap"

stub_dir="$(mktemp -d)"
cat >"${stub_dir}/oc-csv" <<'EOF'
#!/bin/bash
if [[ "$1" == "get" && "$2" == "csv" ]]; then
  echo "clusterserviceversion.operators.coreos.com/koku-service-operator.v0.0.1"
  exit 0
fi
exit 0
EOF
chmod +x "${stub_dir}/oc-csv"
assert_contains "$(olm_existing_csvs "${stub_dir}/oc-csv" cost-demo)" "koku-service-operator.v0.0.1" "existing koku CSVs are detected"

cat >"${stub_dir}/oc-nocsv" <<'EOF'
#!/bin/bash
if [[ "$1" == "get" && "$2" == "csv" ]]; then
  echo "clusterserviceversion.operators.coreos.com/something-else.v1.0.0"
  exit 0
fi
exit 0
EOF
chmod +x "${stub_dir}/oc-nocsv"
assert_eq "$(olm_existing_csvs "${stub_dir}/oc-nocsv" cost-demo)" "" "unrelated CSVs are ignored"
rm -rf "$stub_dir"

grep -Fq 'if [[ "$SOURCE" == "registry" ]]' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: registry source must branch before the build steps" >&2
  fail=1
}
grep -Fq 'olm_install' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: script must call olm_install for the registry source" >&2
  fail=1
}
grep -Fq 'can-i create catalogsources -n openshift-marketplace' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: registry source must pre-check marketplace permissions" >&2
  fail=1
}

# --- Task 5 (#4): architecture discovery and image-arch checks ---
archfx="$(mktemp -d)"
printf 'amd64 arm64\n' >"${archfx}/quay.io_example_multi_v1.txt"
printf 'amd64\n' >"${archfx}/quay.io_example_single_v1.txt"

out="$(IMAGE_ARCH_FIXTURE="$archfx" image_arches quay.io/example/multi:v1)"
assert_eq "$out" "amd64 arm64" "multi-arch image detected"
out="$(IMAGE_ARCH_FIXTURE="$archfx" image_arches quay.io/example/single:v1)"
assert_eq "$out" "amd64" "single-arch image detected"
out="$(IMAGE_ARCH_FIXTURE="$archfx" image_arches quay.io/example/missing:v1)"
assert_eq "$out" "unknown" "missing fixture degrades to unknown"

arch_out="$(IMAGE_ARCH_FIXTURE="$archfx" assert_arch_compatible arm64 quay.io/example/single:v1 2>&1)" && arch_rc=0 || arch_rc=$?
assert_eq "$arch_rc" "1" "arch mismatch fails"
assert_contains "$arch_out" "quay.io/example/single:v1" "mismatch names the image"
assert_contains "$arch_out" "#219" "mismatch references upstream #219"
assert_contains "$arch_out" "ARCH_CHECK=0" "mismatch names the escape hatch"

arch_out="$(IMAGE_ARCH_FIXTURE="$archfx" assert_arch_compatible arm64 quay.io/example/missing:v1 2>&1)" && unverify_rc=0 || unverify_rc=$?
assert_eq "$unverify_rc" "0" "unverifiable image does not fail"
assert_contains "$arch_out" "could not verify" "unverifiable image warns"

arch_out="$(IMAGE_ARCH_FIXTURE="$archfx" assert_arch_compatible amd64 quay.io/example/multi:v1 2>&1)" && ok_rc=0 || ok_rc=$?
assert_eq "$ok_rc" "0" "compatible image passes"
assert_not_contains "$arch_out" "warning" "compatible image passes silently"

stub_dir="$(mktemp -d)"
mk_oc_arch() {
  cat >"$1" <<EOF
#!/bin/bash
if [[ "\$1" == "get" && "\$2" == "nodes" ]]; then
  printf '%b\n' '$2'
  exit 0
fi
exit 0
EOF
  chmod +x "$1"
}
mk_oc_arch "${stub_dir}/oc-amd64" 'amd64'
mk_oc_arch "${stub_dir}/oc-mixed" 'amd64\narm64'
mk_oc_arch "${stub_dir}/oc-s390x" 's390x'

out="$(cluster_node_arch "${stub_dir}/oc-amd64")"
assert_eq "$out" "amd64" "cluster arch detected"
mixed_out="$(cluster_node_arch "${stub_dir}/oc-mixed" 2>/dev/null)" || true
assert_eq "$mixed_out" "amd64" "mixed arch uses the first node"
mixed_warn="$(cluster_node_arch "${stub_dir}/oc-mixed" 2>&1 >/dev/null)"
assert_contains "$mixed_warn" "mixed-architecture" "mixed arch warns"

s390x_out="$(cluster_node_arch "${stub_dir}/oc-s390x" 2>&1)" && s390x_rc=0 || s390x_rc=$?
assert_eq "$s390x_rc" "1" "unsupported arch fails"
assert_contains "$s390x_out" "unsupported" "unsupported arch message"

# CR workload image extraction
tmp_cr="$(mktemp)"
render_preprod_cr "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" "$tmp_cr" \
  "apps.example.test" "https://keycloak-keycloak.apps.example.test" "catalog-demo" "catalog-cost"
cr_imgs="$(cr_workload_images "$tmp_cr")"
assert_contains "$cr_imgs" "quay.io/project-koku/koku:422f758" "CR images include koku"
assert_contains "$cr_imgs" "quay.io/project-koku/insights-rbac:34e25ed" "CR images include insights-rbac"
assert_contains "$cr_imgs" "registry.redhat.io/rhceph/oauth2-proxy-rhel9:v7.6.0" "CR images include oauth2-proxy"
assert_eq "$(printf '%s\n' "$cr_imgs" | grep -c .)" "7" "CR yields seven workload image slots (koku api + masu)"
rm -f "$tmp_cr"

# installed operator image from the CSV (stub oc)
cat >"${stub_dir}/oc-opimg" <<'EOF'
#!/bin/bash
if [[ "$2" == "csv" ]]; then
  echo "clusterserviceversion.operators.coreos.com/koku-service-operator.v0.0.1"
  exit 0
fi
if [[ "$1" == "get" ]]; then
  echo "quay.io/project-koku/koku-service-operator@sha256:deadbeef"
  exit 0
fi
exit 0
EOF
chmod +x "${stub_dir}/oc-opimg"
out="$(installed_operator_image "${stub_dir}/oc-opimg" cost-demo)"
assert_eq "$out" "quay.io/project-koku/koku-service-operator@sha256:deadbeef" "operator image read from the CSV"
rm -rf "$stub_dir" "$archfx"

grep -Fq 'assert_arch_compatible "$NODE_ARCH"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: preflight must assert image arch compatibility" >&2
  fail=1
}
grep -Fq -- '--platform linux/${NODE_ARCH}' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: docker build must target the discovered arch" >&2
  fail=1
}
grep -Fq 'byoi_pod_images' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: post-deploy pass must check BYOI pod images" >&2
  fail=1
}

if [[ "$fail" -ne 0 ]]; then
  echo "demo-preprod tests FAILED" >&2
  exit 1
fi
echo "demo-preprod tests OK"
