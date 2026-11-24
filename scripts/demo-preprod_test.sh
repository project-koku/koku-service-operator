#!/usr/bin/env bash
# Unit tests for scripts/lib/demo-preprod.bash (no cluster required).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "${ROOT}/scripts/lib/demo-preprod.bash"

# Fixture hooks are gated behind DEMO_ALLOW_FIXTURES (issue #18): this runner
# is the intended user; a production install without the gate fails loudly.
export DEMO_ALLOW_FIXTURES=1

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
assert_contains "$plan_manual" "version:     latest" "manual plan shows the truthful resolved version"
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
grep -Fq -- '--platform linux/${arch}' "${ROOT}/scripts/lib/demo-preprod.bash" || {
  echo "FAIL: container build must target the discovered arch" >&2
  fail=1
}
grep -Fq 'build_operator_image "$tool" "$NODE_ARCH"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: build_docker must pass the discovered arch to build_operator_image" >&2
  fail=1
}
grep -Fq 'byoi_pod_images' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: post-deploy pass must check BYOI pod images" >&2
  fail=1
}

# --- Task 13 (#12): container tool abstraction (docker/podman) ---
toolbin="$(mktemp -d)"
tlog="${toolbin}/log"

mk_docker_stub() {
  cat >"${toolbin}/docker" <<EOF
#!/bin/bash
if [[ "\$1" == "info" ]]; then exit $1; fi
if [[ "\$1" == "buildx" && "\$2" == "version" ]]; then exit $2; fi
printf '%s\n' "\$*" >> "${tlog}"
exit 0
EOF
  chmod +x "${toolbin}/docker"
}
cat >"${toolbin}/podman" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${tlog}"
exit 0
EOF
chmod +x "${toolbin}/podman"

SAVE_PATH="$PATH"
export PATH="${toolbin}:$PATH"

# podman-docker shim emulation: `docker info` works, `docker buildx` does not
mk_docker_stub 0 1
docker_up && shim_rc=0 || shim_rc=1
assert_eq "$shim_rc" "1" "podman-docker shim fails the capability probe"
out="$(container_tool)"
assert_eq "$out" "podman" "shim falls through to podman when present"

rm -f "${toolbin}/podman"
# Isolated PATH: only the stub dir — no real podman from the system PATH.
out="$(cd "$ROOT" && PATH="${toolbin}" "$BASH" -c 'source scripts/lib/demo-preprod.bash && container_tool')"
assert_eq "$out" "none" "shim with no podman yields none"

# capable docker (info and buildx both work)
mk_docker_stub 0 0
out="$(container_tool)"
assert_eq "$out" "docker" "capable docker detected"

cat >"${toolbin}/podman" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${tlog}"
exit 0
EOF
chmod +x "${toolbin}/podman"
out="$(CONTAINER_TOOL=podman container_tool)"
assert_eq "$out" "podman" "CONTAINER_TOOL override wins"
bad_ct="$(CONTAINER_TOOL=bogus container_tool 2>&1)" && ct_rc=0 || ct_rc=$?
assert_eq "$ct_rc" "1" "invalid CONTAINER_TOOL fails"
assert_contains "$bad_ct" "docker or podman" "invalid CONTAINER_TOOL names the valid values"

# command emission
: >"$tlog"
build_operator_image docker amd64 quay.example/op:tag /tmp/fake-root >/dev/null
assert_contains "$(cat "$tlog")" "buildx build --platform linux/amd64 -t quay.example/op:tag --push /tmp/fake-root" "docker emits one buildx --push command"

: >"$tlog"
build_operator_image podman arm64 quay.example/op:tag /tmp/fake-root >/dev/null
pod_log="$(cat "$tlog")"
assert_contains "$pod_log" "build --platform linux/arm64 -t quay.example/op:tag /tmp/fake-root" "podman emits build"
assert_contains "$pod_log" "push quay.example/op:tag" "podman emits push"

build_operator_image none amd64 img root >/dev/null 2>&1 && none_rc=0 || none_rc=$?
assert_eq "$none_rc" "1" "no tool fails the build"
rm -rf "$toolbin"
export PATH="$SAVE_PATH"

grep -Fq 'container_tool' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: auto mode must pick the container tool via container_tool" >&2
  fail=1
}
grep -Fq 'build_operator_image' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: build_docker must delegate to build_operator_image" >&2
  fail=1
}

# --- Task 6 (#9): unified wait and post-install verification ---
vdir="$(mktemp -d)"
cat >"${vdir}/cmsc-alltrue.json" <<'EOF'
{"status":{"phase":"Ready","conditions":[
 {"type":"Available","status":"True","reason":"AllComponentsReady"},
 {"type":"UIReady","status":"True","reason":"OAuthClientSecretReady"},
 {"type":"Progressing","status":"False","reason":"ReconcileComplete"},
 {"type":"Degraded","status":"False","reason":"ReconcileComplete"},
 {"type":"ROSEnabled","status":"False","reason":"Disabled"},
 {"type":"DiscoveryComplete","status":"True","reason":"Discovered"},
 {"type":"DatabaseReady","status":"True","reason":"DatabaseReachable"},
 {"type":"CacheReady","status":"True","reason":"CacheReachable"},
 {"type":"KafkaReady","status":"True","reason":"KafkaContractValid"},
 {"type":"StorageReady","status":"True","reason":"StorageBucketsAccessible"},
 {"type":"AuthenticationReady","status":"True","reason":"OIDCReachable"},
 {"type":"SchemaUpToDate","status":"True","reason":"MigrationComplete"},
 {"type":"RBACWorkerReady","status":"True","reason":"RBACWorkerAvailable"},
 {"type":"RBACReady","status":"True","reason":"RBACAvailable"},
 {"type":"IngressReady","status":"True","reason":"IngressReady"},
 {"type":"GatewayReady","status":"True","reason":"GatewayReady"}
]}}
EOF
cat >"${vdir}/cmsc-bad.json" <<'EOF'
{"status":{"phase":"Degraded","conditions":[
 {"type":"Available","status":"False","reason":"WaitingForRBAC"},
 {"type":"UIReady","status":"True","reason":"OAuthClientSecretReady"},
 {"type":"Progressing","status":"True","reason":"Reconciling"},
 {"type":"Degraded","status":"True","reason":"MigrationFailed"},
 {"type":"KafkaReady","status":"False","reason":"TopicMissing"}
]}}
EOF
mk_oc_cmsc() {
  cat >"$1" <<EOF
#!/bin/bash
if [[ "\$2" == "cmsc" ]]; then cat "$2"; exit 0; fi
exit 0
EOF
  chmod +x "$1"
}
mk_oc_cmsc "${vdir}/oc-good" "${vdir}/cmsc-alltrue.json"
mk_oc_cmsc "${vdir}/oc-bad" "${vdir}/cmsc-bad.json"

good_out="$(verify_conditions "${vdir}/oc-good" cost-demo cost-management)" && good_rc=0 || good_rc=$?
assert_eq "$good_rc" "0" "all-True conditions pass"
assert_contains "$good_out" "Available" "table lists Available"
assert_contains "$good_out" "PASS" "table marks passes"
assert_contains "$good_out" "INFO" "ROSEnabled is informational"
assert_not_contains "$good_out" "FAIL" "no failures on the healthy fixture"

bad_out="$(verify_conditions "${vdir}/oc-bad" cost-demo cost-management)" && bad_rc=0 || bad_rc=$?
assert_eq "$bad_rc" "1" "failing conditions fail"
assert_contains "$bad_out" "FAIL" "table marks failures"
assert_contains "$bad_out" "TopicMissing" "failure row shows the reason"
assert_contains "$bad_out" "KafkaReady" "failure row names the condition"
assert_contains "$bad_out" "MISSING" "absent conditions are reported"
rm -rf "${vdir}"

grep -Fq 'verify_conditions "$KUBECTL"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: main flow must run the CMSC verification table" >&2
  fail=1
}
# --- #15: wait_for_ui never reports success after a failed rollout (behavior test) ---
wbin="$(mktemp -d)"
cat >"${wbin}/oc" <<'EOF'
#!/bin/bash
if [[ "$*" == *"rollout status"* ]]; then
  exit "$ROLLOUT_RC"
fi
if [[ "$*" == *"availableReplicas"* ]]; then
  echo "1"
  exit 0
fi
if [[ "$*" == *" cmsc "* ]]; then
  echo "phase=Progressing"
  exit 0
fi
exit 0
EOF
chmod +x "${wbin}/oc"

export ROLLOUT_RC=1
wout="$(wait_for_ui "${wbin}/oc" cost-demo cost-management https://ui.example 0 2 0 2>&1)" && wrc=0 || wrc=$?
assert_eq "$wrc" "1" "failed rollout fails the wait"
assert_not_contains "$wout" "UI ready" "no success message after a failed rollout"
assert_contains "$wout" "rollout failed" "failure names the rollout"

export ROLLOUT_RC=0
wout="$(wait_for_ui "${wbin}/oc" cost-demo cost-management https://ui.example 0 2 0 2>&1)" && wrc=0 || wrc=$?
assert_eq "$wrc" "0" "successful rollout passes"
assert_contains "$wout" "UI ready" "success message printed"
unset ROLLOUT_RC

# Timeout path: deployment never becomes available; 2s deadline, 0s poll.
cat >"${wbin}/oc" <<'EOF'
#!/bin/bash
if [[ "$*" == *"availableReplicas"* ]]; then
  exit 1
fi
if [[ "$2" == "cmsc" ]]; then
  echo "phase=Progressing"
  exit 0
fi
exit 0
EOF
chmod +x "${wbin}/oc"
wout="$(wait_for_ui "${wbin}/oc" cost-demo cost-management https://ui.example 0 2 0 2>&1)" && wrc=0 || wrc=$?
assert_eq "$wrc" "1" "deadline expiry fails the wait"
assert_contains "$wout" "did not become Available" "timeout message printed"
rm -rf "$wbin"

# --- Task 7 (#5): demo-profile hardening ---
tmp7="$(mktemp)"
render_preprod_cr "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" "$tmp7" \
  "apps.example.test" "https://keycloak-keycloak.apps.example.test" "catalog-demo" "catalog-cost"
rendered7="$(cat "$tmp7")"
assert_not_contains "$rendered7" "IQE_TEST_RUN" "default render drops IQE_TEST_RUN"
assert_not_contains "$rendered7" "CURRENCY_URL" "default render drops CURRENCY_URL"
assert_not_contains "$rendered7" "open.er-api.com" "default render drops the external currency API"
rm -f "$tmp7"

tmp7="$(mktemp)"
DEMO_SMOKE=1 render_preprod_cr "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" "$tmp7" \
  "apps.example.test" "https://keycloak-keycloak.apps.example.test" "catalog-demo" "catalog-cost"
unset DEMO_SMOKE
rendered7="$(cat "$tmp7")"
assert_contains "$rendered7" "IQE_TEST_RUN" "smoke escape hatch keeps IQE_TEST_RUN"
assert_contains "$rendered7" "CURRENCY_URL" "smoke escape hatch keeps CURRENCY_URL"
rm -f "$tmp7"

banner7="$(demo_profile_banner)"
assert_contains "$banner7" "not for production" "banner states the demo profile"
assert_contains "$banner7" "docs/install/production.md" "banner points at the production docs"
assert_contains "$banner7" "PLAINTEXT" "banner lists the insecure settings"

grep -Fq 'REALM_ADMIN_PASSWORD' "${ROOT}/scripts/deploy-rhbk.sh" || {
  echo "FAIL: deploy-rhbk.sh must honor REALM_ADMIN_PASSWORD" >&2
  fail=1
}
grep -Fq 'openssl rand' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: the demo must generate a random realm password" >&2
  fail=1
}
grep -Fq 'realm-admin' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: the demo must store the realm password in a Secret" >&2
  fail=1
}
grep -Fq 'demo_profile_banner' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: the demo must print the profile banner" >&2
  fail=1
}

# --- Task 8 (#6): prereqs-only, skip-prereqs, --cr-file ---
help_out="$(cd "$ROOT" && ./scripts/demo-preprod.sh --help 2>&1)"
assert_contains "$help_out" "--prereqs-only" "help documents --prereqs-only"
assert_contains "$help_out" "--skip-prereqs" "help documents --skip-prereqs"
assert_contains "$help_out" "--cr-file" "help documents --cr-file"

plan_po="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run --prereqs-only 2>&1)"
assert_contains "$plan_po" "stopping after the prerequisites" "prereqs-only plan states it stops"

tmp_cr8="$(mktemp)"
plan_sp="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run --skip-prereqs --cr-file "$tmp_cr8" 2>&1)"
rm -f "$tmp_cr8"
assert_contains "$plan_sp" "CR:          " "skip-prereqs plan shows the user CR"
assert_contains "$plan_sp" "user-provided; rendering skipped" "plan marks the CR as user-provided"
assert_contains "$plan_sp" "BYOI is skipped" "skip-prereqs plan states BYOI is skipped"

bad_combo="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run --prereqs-only --skip-prereqs 2>&1)" && combo_rc=0 || combo_rc=$?
assert_eq "$combo_rc" "2" "prereqs-only + skip-prereqs rejected"
assert_contains "$bad_combo" "mutually exclusive" "combo error names the conflict"

bad_cr="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run --cr-file /nonexistent-cr.yaml 2>&1)" && cr_rc=0 || cr_rc=$?
assert_eq "$cr_rc" "2" "missing --cr-file fails fast"
assert_contains "$bad_cr" "not found" "missing --cr-file error names the file"

grep -Fq 'skip "BYOI skipped (--skip-prereqs)' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: the BYOI step must be gated on SKIP_PREREQS" >&2
  fail=1
}
grep -Fq '"$CR_TO_APPLY"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: the apply step must use the resolved CR (user file or rendered)" >&2
  fail=1
}
grep -Fq 'USER_CR_FILE' "${ROOT}/scripts/demo-preprod.env.example" || {
  echo "FAIL: env example documents the user CR override" >&2
  fail=1
}

# --- Task 9 (#8): single-node profile and crc.env split ---
plan_sn="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run --profile=single-node 2>&1)"
assert_contains "$plan_sn" "profile:     single-node" "profile shows in the plan"
assert_contains "$plan_sn" "Kafka 1 broker + 1 controller" "plan shows the Kafka sizing"
assert_contains "$plan_sn" "context:     <current>" "profile does not pin KUBE_CONTEXT"
assert_not_contains "$plan_sn" "martin_povolny" "plan has no arm64 image overrides"

plan_crc_alias="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run --crc 2>&1)"
assert_eq "$plan_crc_alias" "$plan_sn" "--crc is an alias for --profile=single-node"

if [[ -e "${ROOT}/scripts/demo-preprod.crc.env" ]]; then
  echo "FAIL: demo-preprod.crc.env must be replaced by the opt-in arm64 template" >&2
  fail=1
fi
if [[ ! -f "${ROOT}/scripts/demo-preprod.arm64.env" ]]; then
  echo "FAIL: demo-preprod.arm64.env template must exist" >&2
  fail=1
fi
if grep -qE "load_env_file.*arm64" "${ROOT}/scripts/demo-preprod.sh"; then
  echo "FAIL: no flag may auto-load the arm64 template" >&2
  fail=1
fi
bad_prof="$(cd "$ROOT" && env -u KUBE_CONTEXT ./scripts/demo-preprod.sh --dry-run --profile=bogus 2>&1)" && prof_rc=0 || prof_rc=$?
assert_eq "$prof_rc" "2" "unknown profile rejected"

# --- Task 10 (#7): reset removes OLM objects ---
stub_dir="$(mktemp -d)"
cat >"${stub_dir}/oc-reset" <<EOF
#!/bin/bash
echo "\$*" >> "${stub_dir}/log"
exit 0
EOF
chmod +x "${stub_dir}/oc-reset"
reset_olm_objects "${stub_dir}/oc-reset" cost-demo koku-service-operator-catalog >/dev/null 2>&1
reset_cmds="$(cat "${stub_dir}/log")"
rm -rf "$stub_dir"
assert_contains "$reset_cmds" "delete subscription koku-service-operator -n cost-demo --ignore-not-found" "reset removes the Subscription"
assert_contains "$reset_cmds" "delete csv -l operators.coreos.com/koku-service-operator.cost-demo -n cost-demo --ignore-not-found" "reset removes CSVs by label"
assert_contains "$reset_cmds" "delete operatorgroup koku-service-operator-group -n cost-demo --ignore-not-found" "reset removes the OperatorGroup"
assert_contains "$reset_cmds" "delete catalogsource koku-service-operator-catalog -n openshift-marketplace --ignore-not-found" "reset removes the CatalogSource"

grep -Fq 'reset_olm_objects "$KUBECTL" "$NAMESPACE" "$CATALOG_NAME"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: the reset block must call reset_olm_objects" >&2
  fail=1
}

# --- Task 11 (#10): consolidation — remaining acceptance coverage ---
help_out="$(cd "$ROOT" && ./scripts/demo-preprod.sh --help 2>&1)"
for _flag in --reset --dry-run --no-tmux --rebuild --no-open --version --catalog-repo --channel --source; do
  case "$help_out" in
    *"$_flag"*) ;;
    *) echo "FAIL: help must document $_flag" >&2; fail=1 ;;
  esac
done

# KUBE_CONTEXT defaults to empty (current context), never clusterbot
grep -Fq 'KUBE_CONTEXT="${KUBE_CONTEXT:-}"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: KUBE_CONTEXT must default to the current context" >&2
  fail=1
}

# ARCH_CHECK=0 skip path exists and warns
grep -Fq 'ARCH_CHECK=0 — image architecture checks skipped' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: ARCH_CHECK=0 must warn that checks are skipped" >&2
  fail=1
}

# --cr-file passthrough: the apply step uses the resolved CR, not a fixed path
grep -Fq '"$KUBECTL" apply -f "$CR_TO_APPLY"' "${ROOT}/scripts/demo-preprod.sh" || {
  echo "FAIL: the apply step must use the resolved CR (rendered or user-provided)" >&2
  fail=1
}

# The arm64 template is parseable as an env file (comments + KEY=VALUE only)
while IFS= read -r _line || [[ -n "$_line" ]]; do
  _line="${_line%$'\r'}"
  [[ "$_line" =~ ^[[:space:]]*# ]] && continue
  [[ -z "${_line//[[:space:]]/}" ]] && continue
  [[ "$_line" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]] || {
    echo "FAIL: demo-preprod.arm64.env has a non-assignment line: ${_line}" >&2
    fail=1
  }
done <"${ROOT}/scripts/demo-preprod.arm64.env"

# --- #13: tmux wrapper forwards every flag to the inner run (behavior test) ---
tbin="$(mktemp -d)"
tlog="${tbin}/tmux.log"
cat >"${tbin}/tmux" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${tlog}"
exit 0
EOF
chmod +x "${tbin}/tmux"
fx13="$(mktemp)"
printf 'v0.0.1\n' >"$fx13"
# The outer process exits after start_tmux; the stub captures the inner command.
env -u KUBE_CONTEXT PATH="${tbin}:$PATH" REGISTRY_TAG_FIXTURE="$fx13" \
  ./scripts/demo-preprod.sh --prereqs-only --version v0.0.1 --channel stable >/dev/null 2>&1 || true
inner_cmd="$(grep 'send-keys' "$tlog" | grep demo-preprod.sh | tail -1)"
assert_contains "$inner_cmd" "--prereqs-only" "tmux inner run receives --prereqs-only"
assert_contains "$inner_cmd" "--version" "tmux inner run receives --version"
assert_contains "$inner_cmd" "v0.0.1" "tmux inner run receives the version value"
assert_contains "$inner_cmd" "--channel" "tmux inner run receives --channel"
assert_contains "$inner_cmd" "stable" "tmux inner run receives the channel value"
rm -rf "$tbin" "$fx13"

# --- #14: .local.env overrides the example defaults (behavior test) ---
demo_root14="$(mktemp -d)"
mkdir -p "${demo_root14}/scripts/lib"
cp -p "${ROOT}/scripts/demo-preprod.sh" "${ROOT}/scripts/demo-preprod.env.example" "${demo_root14}/scripts/"
cp -p "${ROOT}/scripts/lib/demo-preprod.bash" "${demo_root14}/scripts/lib/"
printf 'NAMESPACE=custom-ns\nKAFKA_NAMESPACE=custom-kafka\n' >"${demo_root14}/scripts/demo-preprod.local.env"
fx14="$(mktemp)"
printf 'v0.0.1\n' >"$fx14"
plan14="$(cd "${demo_root14}" && env -u KUBE_CONTEXT REGISTRY_TAG_FIXTURE="$fx14" ./scripts/demo-preprod.sh --dry-run 2>&1)"
assert_contains "$plan14" "custom-ns" "local.env NAMESPACE overrides the example default"
assert_contains "$plan14" "custom-kafka" "local.env KAFKA_NAMESPACE overrides the example default"
assert_not_contains "$plan14" "cost-byoi " "example default namespace is not used when overridden"
rm -rf "${demo_root14}" "$fx14"

# --- #16: zero/empty availableReplicas is NOT ready (behavior test) ---
rbin="$(mktemp -d)"
mk_oc_avail() {
  cat >"${rbin}/oc" <<EOF
#!/bin/bash
if [[ "\$*" == *"availableReplicas"* ]]; then
  printf '%s' "$1"
  exit 0
fi
exit 1
EOF
  chmod +x "${rbin}/oc"
}
mk_oc_avail "1"
deploy_available "${rbin}/oc" ns dep && avail_rc=0 || avail_rc=$?
assert_eq "$avail_rc" "0" "one available replica is ready"
mk_oc_avail ""
deploy_available "${rbin}/oc" ns dep && avail_rc=0 || avail_rc=$?
assert_eq "$avail_rc" "1" "EMPTY availableReplicas (zero, omitted by k8s) is not ready"
mk_oc_avail "0"
deploy_available "${rbin}/oc" ns dep && avail_rc=0 || avail_rc=$?
assert_eq "$avail_rc" "1" "explicit zero replicas is not ready"
cat >"${rbin}/oc" <<'EOF'
#!/bin/bash
exit 1
EOF
chmod +x "${rbin}/oc"
deploy_available "${rbin}/oc" ns dep && avail_rc=0 || avail_rc=$?
assert_eq "$avail_rc" "1" "missing deployment is not ready"
rm -rf "$rbin"

# --- #18: fixture hooks refuse to run without the explicit gate ---
gate_fx="$(mktemp)"
printf 'v0.0.1\n' >"$gate_fx"
gate_out="$(env -u DEMO_ALLOW_FIXTURES REGISTRY_TAG_FIXTURE="$gate_fx" \
  bash -c 'source scripts/lib/demo-preprod.bash && registry_list_tags project-koku/koku-service-operator-catalog' 2>&1)" && gate_rc=0 || gate_rc=$?
assert_eq "$gate_rc" "1" "fixture without the gate fails"
assert_contains "$gate_out" "fixture mode is not enabled" "ungated fixture names the problem"
rm -f "$gate_fx"

# --- #17: inputs are validated before interpolation ---
validate_version_hint "v0.0.1" && v17=0 || v17=$?
validate_version_hint "latest" && : || v17=1
validate_version_hint "474d937" && : || v17=1
validate_version_hint "474d9370123456789abcdef0123456789abcdef0" && : || v17=1
validate_version_hint "v0.0.1@sha256:$(printf 'a%.0s' {1..64})" && : || v17=1
validate_version_hint "" && : || v17=1
assert_eq "$v17" "0" "valid version hints pass"
bad_v="$(validate_version_hint $'v0.0.1\nextra: yaml' 2>&1)" && bv17=0 || bv17=$?
assert_eq "$bv17" "1" "multiline version rejected"
bad_v="$(validate_version_hint 'v0.0.1@sha256:short' 2>&1)" && bv17=0 || bv17=$?
assert_eq "$bv17" "1" "short digest rejected"
bad_v="$(validate_version_hint 'a"b' 2>&1)" && bv17=0 || bv17=$?
assert_eq "$bv17" "1" "quoted version rejected"

# hostile REALM_ADMIN_PASSWORD survives as JSON (deploy-rhbk.sh builds it with jq --arg)
pw17='bad"pass\word'
json17="$(jq -n --arg pw "$pw17" '[{"username":"admin","password":$pw}]')"
jq -e --arg pw "$pw17" '.[0].password == $pw' <<<"$json17" >/dev/null \
  && : || { echo "FAIL: jq --arg must round-trip hostile passwords" >&2; fail=1; }
grep -Fq 'jq -n --arg pw' "${ROOT}/scripts/deploy-rhbk.sh" || {
  echo "FAIL: deploy-rhbk.sh must build the default users JSON with jq --arg" >&2
  fail=1
}

# DEMO_* override charset
tmp17="$(mktemp)"
render17_rc=0
DEMO_KOKU_TAG='x"y' render_preprod_cr "${ROOT}/config/samples/byoi/app/costmanagementserviceconfig.yaml" "$tmp17" \
  "apps.example.test" "https://kc.apps.example.test" "ns17" "cr17" >/dev/null 2>&1 || render17_rc=$?
assert_eq "$render17_rc" "1" "override value with a quote fails the render"
rm -f "$tmp17"

# --- #19: resolved versions are pinned by digest when known ---
fx19="$(mktemp)"
printf 'main\nv0.0.1 sha256:%s\nv0.0.3 sha256:%s\n' "$(printf 'b%.0s' {1..64})" "$(printf 'c%.0s' {1..64})" >"$fx19"
out="$(REGISTRY_TAG_FIXTURE="$fx19" resolve_catalog_version project-koku/koku-service-operator-catalog "")"
assert_eq "$out" "v0.0.3@sha256:$(printf 'c%.0s' {1..64})" "discovered semver is digest-pinned"
out="$(REGISTRY_TAG_FIXTURE="$fx19" resolve_catalog_version project-koku/koku-service-operator-catalog v0.0.1)"
assert_eq "$out" "v0.0.1@sha256:$(printf 'b%.0s' {1..64})" "explicit tag is digest-pinned"
out="$(REGISTRY_TAG_FIXTURE="$fx19" resolve_catalog_version project-koku/koku-service-operator-catalog main)"
assert_eq "$out" "main" "fixture without a digest stays a bare tag"
out="$(REGISTRY_TAG_FIXTURE="$fx19" resolve_catalog_version project-koku/koku-service-operator-catalog "v0.0.1@sha256:$(printf 'd%.0s' {1..64})")"
assert_eq "$out" "v0.0.1@sha256:$(printf 'd%.0s' {1..64})" "user-supplied digest is preserved verbatim"
rm -f "$fx19"

if [[ "$fail" -ne 0 ]]; then
  echo "demo-preprod tests FAILED" >&2
  exit 1
fi
echo "demo-preprod tests OK"
