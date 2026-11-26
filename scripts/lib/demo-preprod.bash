#!/usr/bin/env bash
# Helpers for scripts/demo-preprod.sh. Sourced; do not execute.
# OpenShift's UBI go-toolset image runs as a random UID. Writing the Go binary
# into WORKDIR /workspace fails with "open manager: permission denied".
patch_operator_dockerfile() {
  local file="${1:?Dockerfile path required}"
  # Portable in-place edit (macOS/Linux).
  local tmp
  tmp="$(mktemp)"
  # Match only the output argument so build tags and package paths survive.
  sed \
    -e '/go build /s| -o manager | -o /tmp/manager |' \
    -e '/go build /s| -o wait-for | -o /tmp/wait-for |' \
    -e 's|go version -m manager wait-for|go version -m /tmp/manager /tmp/wait-for|' \
    -e 's|COPY --from=builder /workspace/manager /manager|COPY --from=builder /tmp/manager /manager|' \
    -e 's|COPY --from=builder /workspace/wait-for /wait-for|COPY --from=builder /tmp/wait-for /wait-for|' \
    "$file" >"$tmp"
  mv "$tmp" "$file"
  # Shipped Dockerfile already writes manager/wait-for/rbac-mini under /tmp|/out;
  # the substitutions above are idempotent no-ops when those paths are present.
}

# Fill the BYOI sample CR: apps domain, public Keycloak issuer, lab TLS skip-verify.
#
# Optional workload image overrides (used by `demo-preprod.sh --crc` for arm64;
# unset on the amd64/clusterbot path, so the rendered CR is byte-identical there):
#   DEMO_KOKU_IMAGE / DEMO_KOKU_TAG              koku api + masu
#   DEMO_RBAC_IMAGE / DEMO_RBAC_TAG              insights-rbac
#   DEMO_INGRESS_IMAGE / DEMO_INGRESS_TAG        ingress
#   DEMO_UI_IMAGE / DEMO_UI_TAG                  koku-ui-onprem

render_preprod_cr() {
  local src="${1:?sample CR path}"
  local dest="${2:?output path}"
  local domain="${3:?cluster apps domain}"
  local issuer="${4:?Keycloak issuer base URL}"
  local namespace="${5:-cost-byoi}"
  local name="${6:-cost-management}"
  python3 - "$src" "$dest" "$domain" "$issuer" "$namespace" "$name" <<'PY'
import os
import sys
from pathlib import Path

src, dest, domain, issuer, namespace, name = sys.argv[1:7]
text = Path(src).read_text()
# Primary substitutions are guarded (review warning #21): a drifted sample
# must fail the render, not silently emit a CR for the wrong namespace/domain.
needle_ns = "  name: cost-management\n  namespace: cost-byoi\n"
if needle_ns not in text:
    raise SystemExit("render_preprod_cr: sample CR name/namespace block not found (drift?)")
needle_dom = 'clusterDomain: "apps.cluster.example.com"'
if needle_dom not in text:
    raise SystemExit("render_preprod_cr: clusterDomain placeholder not found (drift?)")
text = text.replace(needle_ns, f"  name: {name}\n  namespace: {namespace}\n", 1)
text = text.replace(needle_dom, f'clusterDomain: "{domain}"')
old_url = '      url: "http://keycloak-service.keycloak.svc.cluster.local:8080"'
new_block = (
    old_url
    + f'\n      issuerURL: "{issuer}"'
    + "\n      tls:\n        insecureSkipVerify: true"
)
if old_url not in text:
    raise SystemExit("render_preprod_cr: keycloak url line not found in sample CR")
text = text.replace(old_url, new_block, 1)
# Drop the commented placeholders so the live values are the only ones.
text = text.replace('      # issuerURL: "https://keycloak-keycloak.apps.example.com"\n', "")
text = text.replace("      # tls:\n", "")
text = text.replace("      #   insecureSkipVerify: true   # dev only when issuer uses a private CA\n", "")

# Optional per-workload image overrides. Each needle is the verbatim line from
# config/samples/byoi/app/costmanagementserviceconfig.yaml; when the matching
# env var is empty the line is left untouched (amd64 path renders unchanged).
_overrides = [
    ("DEMO_KOKU_IMAGE",       "repository: quay.io/project-koku/koku"),
    ("DEMO_KOKU_TAG",         'tag: "422f758"'),
    ("DEMO_RBAC_IMAGE",       "repository: quay.io/project-koku/insights-rbac"),
    ("DEMO_RBAC_TAG",         'tag: "34e25ed"'),
    ("DEMO_INGRESS_IMAGE",    "repository: quay.io/iop/ingress"),
    ("DEMO_INGRESS_TAG",      'tag: "sha-6dde23d"'),
    ("DEMO_UI_IMAGE",         "repository: quay.io/insights-onprem/koku-ui-onprem"),
    ("DEMO_UI_TAG",           'tag: "d64d393ede6ab625e53c63202a6bcdb6fa48e718"'),
]
for env_key, needle in _overrides:
    val = os.environ.get(env_key, "").strip()
    if not val:
        continue
    # Override values are spliced into YAML and registry URLs: restrict to a
    # safe reference charset (review warning #17).
    import re as _re
    if not _re.fullmatch(r"[A-Za-z0-9._/:@-]+", val):
        raise SystemExit(f"render_preprod_cr: {env_key} has invalid characters (allowed: A-Za-z0-9._/:@-): {val!r}")
    if needle.startswith("repository: "):
        repl = "repository: " + val
    else:
        repl = 'tag: "' + val + '"'
    if needle not in text:
        raise SystemExit(f"render_preprod_cr: {env_key} set but sample line not found: {needle}")
    text = text.replace(needle, repl)

# Demo-profile hardening: the default render drops the CI/test-only env vars
# (IQE keeps them via DEMO_SMOKE=1 or the smoke sample). Strict — a drifted
# sample must fail the render rather than silently leak the vars.
if os.environ.get("DEMO_SMOKE", "") != "1":
    _demo_env_blocks = [
        "      # Live exchange rates for IQE smoke (applied to all koku containers).\n"
        "      # Leave unset for airgapped on-prem.\n"
        "      env:\n"
        '        CURRENCY_URL: "https://open.er-api.com/v6/latest/USD"\n',
        "      # Registers masu test-only routes (/enabled_tags/) for IQE. Lab/CI only.\n"
        "      env:\n"
        '        IQE_TEST_RUN: "True"\n',
    ]
    for block in _demo_env_blocks:
        if block not in text:
            raise SystemExit(
                "render_preprod_cr: demo env block not found in sample CR (drift?): "
                + block.splitlines()[0].strip()
            )
        text = text.replace(block, "")

Path(dest).write_text(text)
PY
}

# Fail with the real oc/kubectl error when the API is down (expired Cluster Bot).
require_reachable_cluster() {
  local kubectl="${1:?kubectl/oc binary}"
  local err
  err="$(mktemp)"
  # whoami is oc-only; kubectl users get a portable authenticated probe.
  if ! "$kubectl" whoami >/dev/null 2>"$err" \
     && ! "$kubectl" get --raw /apis >/dev/null 2>>"$err"; then
    echo "error: kube API is not reachable (context may point at an expired Cluster Bot)" >&2
    sed 's/^/  /' "$err" >&2
    rm -f "$err"
    return 1
  fi
  rm -f "$err"
}

# Print spec.domain from ingress.config.openshift.io/cluster. Do not swallow stderr.
read_apps_domain() {
  local kubectl="${1:?kubectl/oc binary}"
  local err out
  err="$(mktemp)"
  if ! out="$("$kubectl" get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>"$err")"; then
    echo "error: failed to get ingress.config.openshift.io/cluster (need a live OpenShift API)" >&2
    sed 's/^/  /' "$err" >&2
    rm -f "$err"
    return 1
  fi
  rm -f "$err"
  out="${out//[$'\t\r\n ']/}"
  if [[ -z "$out" ]]; then
    echo "error: ingress.config.openshift.io/cluster has an empty spec.domain" >&2
    return 1
  fi
  printf '%s\n' "$out"
}

# Invoke the registry client module (same directory as this file).
_registry_client() {
  python3 "$(dirname "${BASH_SOURCE[0]}")/registry_client.py" "$@"
}

# List the tags of a public quay repository, one per line (paginated).
# REGISTRY_TAG_FIXTURE: when set, read tags from that file instead of HTTP —
# offline tests; production leaves it unset.
registry_list_tags() {
  local repo="${1:?repository path required, e.g. project-koku/koku-service-operator-catalog}"
  if [[ -n "${REGISTRY_TAG_FIXTURE:-}" ]]; then
    fixtures_allowed REGISTRY_TAG_FIXTURE || return 1
    if [[ ! -f "$REGISTRY_TAG_FIXTURE" ]]; then
      echo "error: REGISTRY_TAG_FIXTURE not found: ${REGISTRY_TAG_FIXTURE}" >&2
      return 1
    fi
    cat "$REGISTRY_TAG_FIXTURE"
    return 0
  fi
  local out _tagerr
  _tagerr="$(mktemp)"
  if ! out="$(_registry_client list-tags "$repo" 2>"$_tagerr")"; then
    echo "error: failed to list tags of quay.io/${repo} (registry unreachable or API error)" >&2
    rm -f "$_tagerr"
    return 1
  fi
  # Surface the python's own warnings; swallow tracebacks.
  grep -v -e '^Traceback' -e '^  File ' "$_tagerr" >&2 || true
  rm -f "$_tagerr"
  printf '%s\n' "$out"
}

# Resolve the catalog tag to install. Prints the resolved tag on stdout; an
# optional "@sha256:<digest>" suffix on the hint is preserved verbatim. Fallback
# and passthrough situations print a loud warning on stderr.
#
#   hint empty          -> highest ^v?X.Y.Z tag; none found -> "latest" + warn
#   latest | main       -> passthrough
#   vX.Y.Z | X.Y.Z      -> normalized against the tag list ("v" added if needed)
#   commit SHA (7-40)   -> normalized to the matching short tag
#   anything else       -> must match a tag exactly
#
# An explicit hint that matches nothing is an error (exit 1) listing a few
# available tags; an unreachable registry passes an explicit hint through with
# a warning instead of failing.
resolve_catalog_version() {
  local repo="${1:?repository path required}"
  local hint="${2:-}"
  local tag="${hint%%@*}" digest=""
  if [[ "$hint" == *"@sha256:"* ]]; then
    digest="@${hint#*@}"
  fi

  local tags="" tags_ok=1
  if ! tags="$(registry_list_tags "$repo")"; then
    tags="" tags_ok=0
  else
    # Fixture lines may carry a digest column ("tag sha256:..."); match on
    # the bare tags (pin_tag_digest reads digests separately).
    tags="$(printf '%s\n' "$tags" | awk 'NF {print $1}')"
  fi

  if [[ -z "$tag" ]]; then
    if [[ "$tags_ok" != "1" ]]; then
      warn="could not list tags of quay.io/${repo}; falling back to :latest (a moving target — pin --version for reproducibility)"
    echo "warning: ${warn}" >&2
      printf 'latest\n'
      return 0
    fi
    local best
    best="$(printf '%s\n' "$tags" | python3 -c '
import re, sys
vs = [p for l in sys.stdin for p in l.split()[:1] if re.fullmatch(r"v?\d+\.\d+\.\d+", p)]
print(max(vs, key=lambda v: tuple(int(x) for x in v.lstrip("v").split("."))) if vs else "")
')"
    if [[ -n "$best" ]]; then
      pin_tag_digest "$repo" "$best"
      return 0
    fi
    warn="no semver tag found in quay.io/${repo}; falling back to :latest (a moving target — pin --version for reproducibility)"
    echo "warning: ${warn}" >&2
    pin_tag_digest "$repo" latest
    return 0
  fi

  if [[ "$tag" == "latest" || "$tag" == "main" ]]; then
    pin_tag_digest "$repo" "$tag" "$digest"
    return 0
  fi

  if [[ "$tags_ok" != "1" ]]; then
    warn="could not verify '${tag}' against quay.io/${repo} (registry unreachable); using it as-is"
    echo "warning: ${warn}" >&2
    printf '%s%s\n' "$tag" "$digest"
    return 0
  fi

  if printf '%s\n' "$tags" | grep -Fxq "$tag"; then
    pin_tag_digest "$repo" "$tag" "$digest"
    return 0
  fi
  if printf '%s\n' "$tags" | grep -Fxq "v${tag}"; then
    pin_tag_digest "$repo" "v${tag}" "$digest"
    return 0
  fi
  if [[ "$tag" =~ ^[0-9a-f]{7,40}$ ]]; then
    local m
    m="$(printf '%s\n' "$tags" | awk -v h="$tag" 'index(h, $0) == 1 || index($0, h) == 1' | head -1)"
    if [[ -n "$m" ]]; then
      pin_tag_digest "$repo" "$m" "$digest"
      return 0
    fi
  fi
  echo "error: version '${tag}' not found in quay.io/${repo}." >&2
  echo "  Available tags include: $(printf '%s\n' "$tags" | grep -v '^$' | head -5 | tr '\n' ' ')" >&2
  return 1
}

# Print the OLM objects for the registry source: a CatalogSource in
# openshift-marketplace plus an AllNamespaces OperatorGroup and an Automatic
# Subscription in the application namespace. olm_install applies these; the
# offline tests assert their fields.
olm_manifests() {
  local catalog_img="${1:?catalog image required}"
  local channel="${2:?channel required}"
  local namespace="${3:?namespace required}"
  local catalog_name="${4:-koku-service-operator-catalog}"
  cat <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: ${catalog_name}
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: ${catalog_img}
  displayName: Cost Management
  publisher: Red Hat
  grpcPodConfig:
    securityContextConfig: restricted
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: koku-service-operator-group
  namespace: ${namespace}
# AllNamespaces install mode: no spec target namespaces are set.
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: koku-service-operator
  namespace: ${namespace}
spec:
  channel: ${channel}
  installPlanApproval: Automatic
  name: koku-service-operator
  source: ${catalog_name}
  sourceNamespace: openshift-marketplace
EOF
}

# Print koku-service-operator CSV names in a namespace (empty when none).
olm_existing_csvs() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  "$kubectl" get csv -n "$namespace" -o name 2>/dev/null | grep 'koku-service-operator' || true
}

# Loud warning for an existing install: every published bundle currently
# shares CSV version 0.0.1 (upstream #220), so OLM will not treat a different
# catalog tag as an upgrade — the installed operator stays as-is.
warn_existing_install() {
  local namespace="${1:?namespace required}"
  echo "  warning: a koku-service-operator CSV already exists in ${namespace}." >&2
  echo "  Every published bundle currently shares CSV version 0.0.1, so OLM will not" >&2
  echo "  treat a different catalog tag as an upgrade — the installed operator stays." >&2
  echo "  Run --reset first when you want a different version." >&2
}

# Print the cluster's node architecture: amd64 or arm64. Mixed-architecture
# clusters fail: the demo cannot guarantee pod placement, and proceeding would
# schedule amd64-only images onto arm64 nodes (review note — a green-lit
# broken install is worse than a clear refusal). Anything else is unsupported.
cluster_node_arch() {
  local kubectl="${1:?kubectl/oc binary}"
  local archs first
  archs="$("$kubectl" get nodes -o jsonpath='{range .items[*]}{.status.nodeInfo.architecture}{"\n"}{end}' 2>/dev/null | sort -u | grep -v '^$' || true)"
  if [[ -z "$archs" ]]; then
    echo "error: could not read node architectures (no nodes reported)" >&2
    return 1
  fi
  first="$(printf '%s\n' "$archs" | head -1)"
  if [[ "$(printf '%s\n' "$archs" | grep -c .)" -gt 1 ]]; then
    echo "error: mixed-architecture cluster ($(printf '%s' "$archs" | tr '\n' ' ')). The demo" >&2
    echo "  cannot guarantee pod placement across architectures; use a homogeneous" >&2
    echo "  worker pool." >&2
    return 1
  fi
  case "$first" in
    amd64|arm64)
      printf '%s\n' "$first"
      ;;
    *)
      echo "error: unsupported cluster architecture '${first}' (supported: amd64, arm64)" >&2
      return 1
      ;;
  esac
}

# Print the architectures an image reference supports, space separated, or
# "unknown" when they cannot be determined. IMAGE_ARCH_FIXTURE: when set, arch
# sets are read from ${IMAGE_ARCH_FIXTURE}/<ref with / and : as _>.txt —
# offline tests; production leaves it unset. skopeo is used when installed;
# otherwise a python3 Docker-registry-v2 client (anonymous; public repos).
image_arches() {
  local ref="${1:?image reference required}"
  if [[ -n "${IMAGE_ARCH_FIXTURE:-}" ]]; then
    fixtures_allowed IMAGE_ARCH_FIXTURE || return 1
    local f="${IMAGE_ARCH_FIXTURE}/$(printf '%s' "$ref" | tr '/:' '__').txt"
    if [[ -f "$f" ]]; then
      cat "$f"
    else
      echo "unknown"
    fi
    return 0
  fi
  local out=""
  if command -v skopeo >/dev/null 2>&1; then
    out="$(skopeo inspect --raw "docker://${ref}" 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print("SINGLE"); raise SystemExit
if "manifests" in d:
    archs = set()
    for m in d["manifests"]:
        p = m.get("platform", {})
        if p.get("os", "linux") == "linux":
            archs.add(p.get("architecture", "unknown"))
    print(" ".join(sorted(archs)) if archs else "unknown")
else:
    print("SINGLE")
' 2>/dev/null || true)"
    if [[ "$out" == "SINGLE" ]]; then
      out="$(skopeo inspect "docker://${ref}" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("Architecture","unknown"))' 2>/dev/null || true)"
    fi
    if [[ -n "$out" ]]; then
      printf '%s\n' "$out"
      return 0
    fi
  fi
  out="$(_registry_client image-arches "$ref" 2>/dev/null)" || out="unknown"
  printf '%s\n' "${out:-unknown}"
}

# Fail when any listed image cannot run on the given architecture. Images whose
# arch set could not be determined produce a warning instead of a failure.
assert_arch_compatible() {
  local arch="${1:?architecture required}"
  shift
  local bad=() unverifiable=() img arches
  for img in "$@"; do
    arches="$(image_arches "$img")"
    if [[ " $arches " == *" $arch "* ]]; then
      continue
    fi
    if [[ "$arches" == "unknown" || -z "$arches" ]]; then
      unverifiable+=("$img")
    else
      bad+=("$img (${arches// /, })")
    fi
  done
  if [[ ${#unverifiable[@]} -gt 0 ]]; then
    echo "warning: could not verify architecture for: ${unverifiable[*]}" >&2
  fi
  if [[ ${#bad[@]} -eq 0 ]]; then
    return 0
  fi
  echo "error: cluster architecture is ${arch}, but these images cannot run on it:" >&2
  printf '  - %s\n' "${bad[@]}" >&2
  if [[ "$arch" == "arm64" ]]; then
    echo "  The published v0.0.1 operator and workload images are amd64-only; multi-arch" >&2
    echo "  builds are pending in project-koku/koku-service-operator#219. Pin arm64" >&2
    echo "  rebuilds via the DEMO_*_IMAGE overrides, or set ARCH_CHECK=0 to skip this check." >&2
  else
    echo "  Pin images built for ${arch}, or set ARCH_CHECK=0 to skip this check." >&2
  fi
  return 1
}

# Print the workload image references (repository:tag pairs) of a rendered
# CostManagementServiceConfig.
cr_workload_images() {
  local file="${1:?CR file required}"
  python3 - "$file" <<'PY'
import re, sys

text = open(sys.argv[1]).read()
images, repo = [], None
for line in text.splitlines():
    m = re.match(r"\s*repository:\s*(\S+)\s*$", line)
    if m:
        repo = m.group(1)
        continue
    m = re.match(r'\s*tag:\s*"?([^"\s]+)"?\s*$', line)
    if m and repo:
        images.append(f"{repo}:{m.group(1)}")
        repo = None
print("\n".join(images))
PY
}

# Print the operator image the installed CSV deploys (empty when no CSV).
installed_operator_image() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local csv
  csv="$("$kubectl" get csv -n "$namespace" -o name 2>/dev/null | grep 'koku-service-operator' | head -1 || true)"
  [[ -n "$csv" ]] || return 1
  "$kubectl" get "$csv" -n "$namespace" -o jsonpath='{.spec.install.spec.deployments[0].spec.template.spec.containers[0].image}' 2>/dev/null
}

# Print the unique container images of running pods in a namespace — the
# post-deploy arch pass checks what actually got scheduled.
byoi_pod_images() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  "$kubectl" get pods -n "$namespace" -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{end}' 2>/dev/null | sort -u | grep -v '^$' || true
}

# docker counts as available only when buildx actually works. The
# podman-docker shim (/usr/bin/docker from podman-docker) translates
# `docker info` but forwards `docker buildx` to `podman buildx`, which does
# not exist — so the buildx probe is what distinguishes real docker.
docker_up() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1
}

# Print the container tool to use for local builds: docker|podman|none.
# CONTAINER_TOOL overrides detection (docker or podman).
container_tool() {
  if [[ -n "${CONTAINER_TOOL:-}" ]]; then
    case "$CONTAINER_TOOL" in
      docker|podman)
        printf '%s\n' "$CONTAINER_TOOL"
        return 0
        ;;
      *)
        echo "error: CONTAINER_TOOL must be docker or podman (got '${CONTAINER_TOOL}')" >&2
        return 1
        ;;
    esac
  fi
  if docker_up; then
    printf 'docker\n'
    return 0
  fi
  if command -v podman >/dev/null 2>&1; then
    printf 'podman\n'
    return 0
  fi
  printf 'none\n'
  return 0
}

# Build and push the operator image with the given tool. podman has no
# combined --push, so it builds then pushes (two commands).
build_operator_image() {
  local tool="${1:?tool required}"
  local arch="${2:?architecture required}"
  local img="${3:?image required}"
  local root="${4:?repo root required}"
  case "$tool" in
    docker)
      echo "  docker buildx build --platform linux/${arch} -t ${img} --push"
      docker buildx build --platform "linux/${arch}" -t "$img" --push "$root"
      ;;
    podman)
      echo "  podman build --platform linux/${arch} -t ${img}"
      podman build --platform "linux/${arch}" -t "$img" "$root"
      echo "  podman push ${img}"
      podman push "$img"
      ;;
    *)
      echo "error: BUILD_MODE=docker needs docker (with buildx) or podman; none found." >&2
      echo "  Install podman, or use BUILD_MODE=openshift for an in-cluster build." >&2
      return 1
      ;;
  esac
}

# Print a pass/fail table of the CMSC conditions and exit non-zero when any
# expected condition is unhealthy. Positive conditions pass on True;
# Progressing/Degraded pass on False (their healthy state); ROSEnabled is
# informational (False is expected while ROS is disabled). Absent conditions
# are reported as MISSING and count as failures.
verify_conditions() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local cr="${3:?CR name required}"
  local json
  json="$("$kubectl" get cmsc "$cr" -n "$namespace" -o json 2>/dev/null)" || {
    echo "error: cannot read CMSC ${namespace}/${cr}" >&2
    return 1
  }
  printf '%s\n' "$json" | python3 -c '
import json, sys

d = json.load(sys.stdin)
conds = {c["type"]: c for c in d.get("status", {}).get("conditions", [])}
positive = ["Available", "UIReady", "DiscoveryComplete", "DatabaseReady",
            "CacheReady", "KafkaReady", "StorageReady", "AuthenticationReady",
            "SchemaUpToDate", "RBACReady", "IngressReady", "GatewayReady"]
negative = ["Progressing", "Degraded"]
fails = 0

def row(name, state, detail):
    print(f"  {name:<18} {state:<5} {detail}")

print("CMSC verification:")
for t in positive:
    c = conds.get(t)
    if c is None:
        row(t, "FAIL", "MISSING"); fails += 1
    elif c.get("status") == "True":
        row(t, "PASS", "True (%s)" % c.get("reason", ""))
    else:
        row(t, "FAIL", "%s (%s)" % (c.get("status"), c.get("reason", ""))); fails += 1
for t in negative:
    c = conds.get(t)
    if c is None:
        row(t, "FAIL", "MISSING"); fails += 1
    elif c.get("status") == "False":
        row(t, "PASS", "False (%s)" % c.get("reason", ""))
    else:
        row(t, "FAIL", "%s (%s)" % (c.get("status"), c.get("reason", ""))); fails += 1
# Informational conditions: present in status but explicitly non-gating
# (RBACWorkerReady per the CRD docs) or contextual (ROSEnabled while ROS is
# off, CoreServicesReady is a roll-up) — printed, never fatal.
for _info in ("ROSEnabled", "RBACWorkerReady", "CoreServicesReady"):
    c = conds.get(_info)
    if c is not None:
        row(_info, "INFO", "%s (%s)" % (c.get("status"), c.get("reason", "")))
sys.exit(1 if fails else 0)
'
}

# Python snippet executed inside the koku-api pod to provision the demo
# tenant. Kept as a generator so the offline test suite can assert its
# content without a cluster. Mirrors the provisioning a first source add
# triggers in production code (IdentityHeaderMiddleware.create_customer +
# ProviderBuilder._tenant_for_schema): Customer row, Tenant row, and the
# org schema cloned from the tenant template.
tenant_bootstrap_python() {
  local org_id="${1:?org_id required}"
  local account="${2:?account number required}"
  cat <<PYEOF
from django.db import connection
from django_tenants.utils import schema_exists

from api.iam.models import Tenant
from api.iam.serializers import create_schema_name
from koku.middleware import IdentityHeaderMiddleware

ORG_ID = "${org_id}"
ACCOUNT = "${account}"

schema = create_schema_name(ORG_ID)
customer = IdentityHeaderMiddleware.create_customer(ACCOUNT, ORG_ID, "POST")
tenant, _ = Tenant.objects.get_or_create(schema_name=schema)
if not schema_exists(schema):
    # First run builds the tenant template schema first (several minutes of
    # migrations); re-runs skip straight past this.
    tenant.create_schema()
assert schema_exists(schema), "tenant schema %s was not created" % schema
with connection.cursor() as cur:
    cur.execute("SELECT count(*) FROM {}.enabled_currency".format(schema))
    seeded = cur.fetchone()[0]
    assert seeded > 0, "enabled_currency seed missing in %s" % schema
connection.set_schema_to_public()
print("BOOTSTRAP OK schema=%s currencies=%d" % (schema, seeded))
PYEOF
}

# Provision the demo tenant (Customer row + org schema) in the koku-api pod.
# A fresh on-prem install has NO tenant until the first source is added, so
# every request falls back to the public schema and tenant-scoped features
# are broken out of the box: Settings > Display shows "temporarily
# unavailable" for data retention (503) and an empty currency dropdown.
# Idempotent: an existing schema is left untouched.
bootstrap_tenant() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local cr_name="${3:?CR name required}"
  local org_id="${4:?org_id required}"
  local account="${5:?account number required}"
  local deploy="${cr_name}-koku-api"
  local var
  for var in "$org_id" "$account"; do
    if [[ ! "$var" =~ ^[A-Za-z0-9_-]+$ ]]; then
      echo "error: DEMO_ORG_ID/DEMO_ACCOUNT_NUMBER must be alphanumeric (got: ${var})" >&2
      return 1
    fi
  done
  local out=""
  if ! out="$(tenant_bootstrap_python "$org_id" "$account" \
      | "$kubectl" exec -i -n "$namespace" "deploy/${deploy}" \
        -- sh -c 'cd /opt/koku/koku && python manage.py shell' 2>&1)"; then
    printf '%s\n' "$out" >&2
    echo "error: tenant bootstrap failed (${namespace}/deploy/${deploy}) — log above" >&2
    return 1
  fi
  if ! grep -q "BOOTSTRAP OK" <<<"$out"; then
    printf '%s\n' "$out" >&2
    echo "error: tenant bootstrap did not complete (no BOOTSTRAP OK marker) — log above" >&2
    return 1
  fi
  grep "BOOTSTRAP OK" <<<"$out" | tail -1
}

# Post-install API smoke check through the koku-api pod: the two endpoints
# that require a provisioned tenant. Catches a missing or broken tenant at
# install time instead of hours later as an empty Settings page.
smoke_tenant_api() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local cr_name="${3:?CR name required}"
  local org_id="${4:?org_id required}"
  local account="${5:?account number required}"
  local deploy="${cr_name}-koku-api"
  local identity
  identity="$(printf '{"identity":{"account_number":"%s","org_id":"%s","type":"User","user":{"username":"admin","email":"admin@demo.io","is_org_admin":true}},"entitlements":{"cost_management":{"is_entitled":true}}}' \
    "$account" "$org_id" | base64 -w0)"
  local api="http://localhost:8000/api/cost-management/v1"
  local body err_file fails=0
  # oc exec chatter ("Defaulted container …") goes to stderr and would break
  # JSON parsing if merged into the body; keep it for the failure message.
  err_file="$(mktemp)"

  echo "API smoke check (tenant ${org_id}):"
  body="$("$kubectl" exec -n "$namespace" "deploy/${deploy}" -- curl -s \
    -H "x-rh-identity: ${identity}" "${api}/account-settings/data-retention/" 2>"$err_file")" || true
  if printf '%s' "$body" | python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if isinstance(d.get("data_retention_months"), int) else 1)' 2>/dev/null; then
    echo "  data-retention     PASS  $(printf '%s' "$body" | tr -d '\n' | head -c 120)"
  else
    echo "  data-retention     FAIL  ${body:-$(tr -d '\n' <"$err_file" | head -c 120)}" >&2
    fails=1
  fi

  body="$("$kubectl" exec -n "$namespace" "deploy/${deploy}" -- curl -s \
    -H "x-rh-identity: ${identity}" "${api}/settings/currency/?filter%5Benabled%5D=true" 2>"$err_file")" || true
  if printf '%s' "$body" | python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if d.get("meta", {}).get("count", 0) > 0 else 1)' 2>/dev/null; then
    echo "  currency list      PASS  $(printf '%s' "$body" | python3 -c 'import json,sys; print("%d enabled" % json.load(sys.stdin)["meta"]["count"])' 2>/dev/null)"
  else
    echo "  currency list      FAIL  ${body:-$(tr -d '\n' <"$err_file" | head -c 120)}" >&2
    fails=1
  fi
  rm -f "$err_file"
  return "$fails"
}

# --- Teardown helpers (--reset / --uninstall; fork issues #37/#38) ----------
# Ordering rule for everything below: CRs must be gone before their CRDs
# (CRD deletion hangs on CR finalizers), and namespaces before anything that
# lives inside them. All deletes are --ignore-not-found and safe to re-run.

# Remove the CMSC CRD and the operator's admission webhooks. Both install
# paths can leave these: OLM (CSV-owned) and the local path (deploy-dev.sh
# applies the CRD; the webhooks come from the CSV or a kustomize apply).
# Deletes use --wait=false: `oc delete crd` blocks until every CR is
# garbage-collected, which is an unbounded hang when one is still present
# (observed live). The bounded wait below then confirms the CRD is really
# gone — a lingering deleting-CRD breaks the next install's apply.
delete_koku_crd() {
  local kubectl="${1:?kubectl/oc binary}"
  local crd=costmanagementserviceconfigs.service.costmanagement.openshift.io
  echo "  removing CMSC CRD and admission webhooks…"
  "$kubectl" delete crd "$crd" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  "$kubectl" delete mutatingwebhookconfiguration koku-service-operator-mutating-webhook-configuration \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  "$kubectl" delete validatingwebhookconfiguration koku-service-operator-validating-webhook-configuration \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # The CRD completes deletion once its CRs are gone (the caller guarantees
  # the CMSC CR was deleted first).
  local tries=0
  while (( tries < 12 )); do
    "$kubectl" get crd "$crd" >/dev/null 2>&1 || return 0
    sleep 5
    tries=$((tries + 1))
  done
  echo "error: CMSC CRD still present after 60s — a leftover CR may be blocking it:" >&2
  echo "  ${kubectl} get costmanagementserviceconfigs -A" >&2
  return 1
}

# Remove the local-path operator RBAC applied by scripts/deploy-dev.sh:
# ClusterRoleBindings (cluster-scoped, fixed names) and the ClusterRoles they
# reference. The leader-election RoleBinding is namespaced and dies with the
# app namespace. OLM-installed roles carry CSV-hash names and are untouched.
delete_operator_rbac() {
  local kubectl="${1:?kubectl/oc binary}"
  local ns="${2:?namespace required}"
  echo "  removing operator RBAC (deploy-dev bindings)…"
  "$kubectl" delete clusterrolebinding koku-operator-dev koku-operator-dev-cluster \
    --ignore-not-found >/dev/null 2>&1 || true
  "$kubectl" delete rolebinding koku-operator-dev-leader-election -n "$ns" \
    --ignore-not-found >/dev/null 2>&1 || true
  "$kubectl" delete clusterrole manager-role manager-cluster-role \
    --ignore-not-found >/dev/null 2>&1 || true
}

# Drop and recreate the Cost Management databases in the RETAINED postgres
# (instance reset keeps the deployment running, so nothing re-runs the
# first-boot db-init). Recreating them empty with the original owners is what
# lets a re-install's migration job succeed without a postgres restart —
# including the pg_stat_statements extension the db-init installs as the
# superuser (koku's migration needs it and cannot create it itself; observed
# live as MigrationFailed "permission denied to create extension").
# The credentials secret lives in the APP namespace (deleted later in the
# reset) while postgres runs in the infra namespace — hence the two
# arguments. Call this BEFORE the app namespace deletion: WITH (FORCE)
# terminates the applications' connections. The password travels via stdin
# (oc exec argv is visible in `ps` on the host — review #24 hygiene).
recreate_demo_databases() {
  local kubectl="${1:?kubectl/oc binary}"
  local secret_ns="${2:?credentials namespace required}"
  local db_ns="${3:?database namespace required}"
  local user koku_role rbac_role ros_role kruize_role
  user="$("$kubectl" get secret byoi-db-credentials -n "$secret_ns" -o jsonpath='{.data.postgres-user}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  if [[ -z "$user" ]]; then
    echo "  skipping database wipe: byoi-db-credentials not found in ${secret_ns} (infra not demo-owned or already gone)"
    return 0
  fi
  koku_role="$("$kubectl" get secret byoi-db-credentials -n "$secret_ns" -o jsonpath='{.data.koku-user}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  rbac_role="$("$kubectl" get secret byoi-db-credentials -n "$secret_ns" -o jsonpath='{.data.rbac-user}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  ros_role="$("$kubectl" get secret byoi-db-credentials -n "$secret_ns" -o jsonpath='{.data.ros-user}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  kruize_role="$("$kubectl" get secret byoi-db-credentials -n "$secret_ns" -o jsonpath='{.data.kruize-user}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  local role
  for role in "$user" "$koku_role" "$rbac_role" "$ros_role" "$kruize_role"; do
    if [[ ! "$role" =~ ^[a-zA-Z0-9_]+$ ]]; then
      echo "error: unexpected role name in byoi-db-credentials — refusing to run SQL" >&2
      return 1
    fi
  done
  echo "  dropping + recreating Cost Management databases in ${db_ns}…"
  local pass sql
  pass="$("$kubectl" get secret byoi-db-credentials -n "$secret_ns" -o jsonpath='{.data.postgres-password}' 2>/dev/null | base64 -d 2>/dev/null)"
  sql="DROP DATABASE IF EXISTS costonprem_koku WITH (FORCE); CREATE DATABASE costonprem_koku OWNER ${koku_role};
\\c costonprem_koku
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
DROP DATABASE IF EXISTS costonprem_rbac WITH (FORCE); CREATE DATABASE costonprem_rbac OWNER ${rbac_role};
DROP DATABASE IF EXISTS costonprem_ros WITH (FORCE); CREATE DATABASE costonprem_ros OWNER ${ros_role};
DROP DATABASE IF EXISTS costonprem_kruize WITH (FORCE); CREATE DATABASE costonprem_kruize OWNER ${kruize_role};"
  if ! printf '%s\n%s\n' "$pass" "$sql" | "$kubectl" exec -i -n "$db_ns" deploy/postgresql -- \
      sh -c 'read -r pgpass; PGPASSWORD="$pgpass" psql -U "'"$user"'" -d postgres -v ON_ERROR_STOP=1' >/dev/null 2>&1; then
    echo "error: database drop/recreate failed in ${db_ns}/deploy/postgresql" >&2
    return 1
  fi
}

# Delete the Cost Management Kafka topics (label-scoped to OUR Kafka cluster —
# a shared namespace with foreign topics on other clusters is not touched).
# The cluster itself and the AMQ Streams operator are kept by the instance
# reset; deploy-kafka.sh recreates missing topics on re-install.
delete_demo_kafka_topics() {
  local kubectl="${1:?kubectl/oc binary}"
  local ns="${2:?namespace required}"
  local cluster="${3:?kafka cluster name required}"
  if ! "$kubectl" -n "$ns" get kafka "$cluster" >/dev/null 2>&1; then
    echo "  skipping Kafka topic deletion: kafka/${cluster} not found in ${ns}"
    return 0
  fi
  echo "  deleting Cost Management Kafka topics (cluster ${cluster})…"
  "$kubectl" delete kafkatopic -n "$ns" -l "strimzi.io/cluster=${cluster}" \
    --ignore-not-found --timeout=120s >/dev/null 2>&1 || true
}

# Delete the Cost Management realm via the Keycloak Admin API, keeping the
# Keycloak instance and the RHBK operator. Deleting the keycloakrealmimport
# CR does NOT delete the realm — the API call is required. deploy-rhbk.sh
# re-imports the realm on the next install. Sensitive values (password,
# bearer token) travel via temp files, never curl argv (review #24).
delete_keycloak_realm() {
  local kubectl="${1:?kubectl/oc binary}"
  local ns="${2:?namespace required}"
  local realm="${3:?realm name required}"
  if [[ ! "$realm" =~ ^[a-zA-Z0-9_-]+$ ]]; then
    echo "error: invalid realm name '${realm}'" >&2
    return 1
  fi
  local user pass host
  user="$("$kubectl" get secret keycloak-initial-admin -n "$ns" -o jsonpath='{.data.username}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  pass="$("$kubectl" get secret keycloak-initial-admin -n "$ns" -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  host="$("$kubectl" get route keycloak -n "$ns" -o jsonpath='{.spec.host}' 2>/dev/null)" || true
  if [[ -z "$user" || -z "$pass" || -z "$host" ]]; then
    echo "  skipping realm deletion: keycloak-initial-admin secret or keycloak route not found in ${ns}"
    return 0
  fi
  echo "  deleting Keycloak realm '${realm}'…"
  local tls=(-k)
  local ca=""
  if [[ "${RHBK_INSECURE_TLS:-0}" != "1" ]]; then
    ca="$(mktemp "${TMPDIR:-/tmp}/realm-ca-XXXXXX")"
    if "$kubectl" get secret router-ca -n openshift-ingress-operator \
        -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d >"$ca" 2>/dev/null && [[ -s "$ca" ]]; then
      tls=(--cacert "$ca")
    else
      rm -f "$ca"; ca=""
      echo "  warning: cluster router CA not found; falling back to insecure TLS for the realm API call" >&2
    fi
  fi
  local body token status rc=0
  body="$(mktemp "${TMPDIR:-/tmp}/realm-body-XXXXXX")"
  python3 - "$body" "$user" "$pass" <<'PY'
import sys, urllib.parse
path, user, pw = sys.argv[1:4]
open(path, "w").write(
    "grant_type=password&client_id=admin-cli&"
    + urllib.parse.urlencode({"username": user, "password": pw})
)
PY
  token="$(curl -s --max-time 30 "${tls[@]}" -X POST \
    "https://${host}/realms/master/protocol/openid-connect/token" \
    --data "@${body}" 2>/dev/null | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("access_token", ""))
except Exception:
    print("")')"
  rm -f "$body"
  if [[ -z "$token" ]]; then
    echo "error: could not obtain a Keycloak admin token (realm ${realm} NOT deleted)" >&2
    [[ -n "$ca" ]] && rm -f "$ca"
    return 1
  fi
  local hdr
  hdr="$(mktemp "${TMPDIR:-/tmp}/realm-hdr-XXXXXX")"
  printf 'Authorization: Bearer %s\n' "$token" >"$hdr"
  status="$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 "${tls[@]}" -X DELETE \
    --header "@${hdr}" "https://${host}/admin/realms/${realm}" 2>/dev/null)" || status=000
  rm -f "$hdr"
  [[ -n "$ca" ]] && rm -f "$ca"
  case "$status" in
    2*)
      echo "  realm '${realm}' deleted"
      ;;
    404)
      echo "  realm '${realm}' already gone (404)"
      ;;
    *)
      echo "error: realm deletion returned HTTP ${status} (realm ${realm} NOT deleted)" >&2
      rc=1
      ;;
  esac
  return "$rc"
}

# Flush the retained Valkey (stale cache keys from the previous install).
flush_valkey() {
  local kubectl="${1:?kubectl/oc binary}"
  local ns="${2:?namespace required}"
  local pass
  pass="$("$kubectl" get secret valkey-credentials -n "$ns" -o jsonpath='{.data.redis-password}' 2>/dev/null | base64 -d 2>/dev/null)" || true
  if [[ -z "$pass" ]]; then
    echo "  skipping Valkey flush: valkey-credentials not found in ${ns}"
    return 0
  fi
  echo "  flushing Valkey cache…"
  printf '%s\n' "$pass" | "$kubectl" exec -i -n "$ns" deploy/valkey -- \
    sh -c 'read -r vp; redis-cli -a "$vp" --no-auth-warning FLUSHALL' >/dev/null 2>&1 \
    || echo "  warning: Valkey FLUSHALL failed (continuing)" >&2
}

# Clear the object-storage buckets (previous install's uploaded payloads).
# Runs the same pinned mc image as the minio-init job: the MinIO server image
# does not ship mc. Buckets are removed and recreated empty so the state is
# pristine even if a later install skips minio-init.
clear_minio() {
  local kubectl="${1:?kubectl/oc binary}"
  local ns="${2:?namespace required}"
  if ! "$kubectl" -n "$ns" get deploy minio >/dev/null 2>&1; then
    echo "  skipping MinIO clear: deploy/minio not found in ${ns}"
    return 0
  fi
  echo "  clearing MinIO buckets…"
  if ! "$kubectl" apply -f - >/dev/null 2>&1 <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: minio-clear
  namespace: ${ns}
spec:
  ttlSecondsAfterFinished: 600
  backoffLimit: 3
  template:
    spec:
      restartPolicy: OnFailure
      containers:
        - name: mc
          image: docker.io/pgsty/mc:latest@sha256:cfc83108c3abb371f8fb84d99c1fdc88f8c237e022409b0081fb7c0a3be634dd
          imagePullPolicy: IfNotPresent
          env:
            - name: HOME
              value: /tmp
            - name: MC_CONFIG_DIR
              value: /tmp/.mc
            - name: MINIO_ROOT_USER
              valueFrom:
                secretKeyRef:
                  name: minio-credentials
                  key: access-key
            - name: MINIO_ROOT_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: minio-credentials
                  key: secret-key
          command: ["sh", "-c"]
          args:
            - |
              set -euo pipefail
              until mc alias set local http://minio:9000 "\$MINIO_ROOT_USER" "\$MINIO_ROOT_PASSWORD"; do sleep 3; done
              for b in koku-bucket koku-upload-bucket ros-data; do
                mc rb --force "local/\$b" 2>/dev/null || true
                mc mb --ignore-existing "local/\$b"
              done
              echo "buckets cleared"
EOF
  then
    echo "error: could not create the minio-clear Job" >&2
    return 1
  fi
  if ! "$kubectl" -n "$ns" wait --for=condition=complete job/minio-clear --timeout=180s >/dev/null 2>&1; then
    echo "  warning: minio-clear job did not complete in time — stale objects may remain" >&2
  fi
  "$kubectl" -n "$ns" delete job minio-clear --ignore-not-found >/dev/null 2>&1 || true
}

# --- --uninstall helpers (full teardown; fork issue #38) ---------------------

# Remove the AMQ Streams operator: its Subscription lives in
# openshift-operators (NOT the kafka namespace), so deleting the namespace
# leaves it behind — the original --reset gap.
delete_amq_streams_operator() {
  local kubectl="${1:?kubectl/oc binary}"
  echo "  removing AMQ Streams operator (openshift-operators)…"
  "$kubectl" delete subscription amq-streams -n openshift-operators \
    --ignore-not-found >/dev/null 2>&1 || true
  "$kubectl" delete csv -n openshift-operators \
    -l operators.coreos.com/amq-streams.openshift-operators \
    --ignore-not-found >/dev/null 2>&1 || true
}

# Remove the RHBK operator objects (Subscription + OperatorGroup in the
# keycloak namespace; the namespace deletion usually takes them, this makes
# it explicit and covers a renamed namespace).
delete_rhbk_operator() {
  local kubectl="${1:?kubectl/oc binary}"
  local ns="${2:?namespace required}"
  echo "  removing RHBK operator objects…"
  "$kubectl" delete subscription rhbk-operator -n "$ns" \
    --ignore-not-found >/dev/null 2>&1 || true
  "$kubectl" delete operatorgroup rhbk-operator-group -n "$ns" \
    --ignore-not-found >/dev/null 2>&1 || true
}

# Remove the Kafka and Keycloak CRDs (after their CRs and namespaces are
# gone — CRD deletion hangs on CR finalizers otherwise). Only CRDs from the
# strimzi (both API groups: kafka.strimzi.io AND core.strimzi.io — the
# strimzipodsets CRD lives in the latter) and keycloak API groups are
# touched; everything else is listed and skipped.
delete_infra_crds() {
  local kubectl="${1:?kubectl/oc binary}"
  local crd
  echo "  removing Kafka and Keycloak CRDs…"
  while IFS= read -r crd; do
    [[ -z "$crd" ]] && continue
    "$kubectl" delete "$crd" --ignore-not-found >/dev/null 2>&1 || true
  done < <("$kubectl" get crd -o name 2>/dev/null \
    | grep -E '\.(kafka\.strimzi\.io|core\.strimzi\.io|k8s\.keycloak\.org)$' || true)
}

# Demo/evaluation profile banner: this installer is not for production, and the
# summary lists the insecure settings the demo profile uses.
demo_profile_banner() {
  cat <<'EOF'
Demo/evaluation profile — not for production.
  Insecure settings in use: demo realm user, Kafka PLAINTEXT, sslMode=disable,
  OIDC insecureSkipVerify. For production see docs/install/production.md.
EOF
}

# Remove the OLM objects a registry-source install created: Subscription,
# CSVs (by OLM's package label), OperatorGroup in the app namespace, and the
# CatalogSource in openshift-marketplace. Called by --reset AFTER the CMSC
# deletion (the operator must survive long enough to clear its finalizer).
reset_olm_objects() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local catalog_name="${3:?catalog source name required}"
  echo "  removing OLM objects (Subscription, CSV, OperatorGroup, CatalogSource)…"
  "$kubectl" delete subscription koku-service-operator -n "$namespace" --ignore-not-found >/dev/null 2>&1 || true
  "$kubectl" delete csv -l "operators.coreos.com/koku-service-operator.${namespace}" -n "$namespace" --ignore-not-found >/dev/null 2>&1 || true
  "$kubectl" delete operatorgroup koku-service-operator-group -n "$namespace" --ignore-not-found >/dev/null 2>&1 || true
  "$kubectl" delete catalogsource "$catalog_name" -n openshift-marketplace --ignore-not-found >/dev/null 2>&1 || true
}

# True when the Deployment exists and has at least one available replica.
# Kubernetes omits .status.availableReplicas when it is zero, so an EMPTY
# result must count as not-ready — the previous `[[ "" != "0" ]]` passed a
# crash-looping deployment as healthy (review critical #16).
deploy_available() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local deployment="${3:?deployment name required}"
  local n
  n="$("$kubectl" -n "$namespace" get deploy "$deployment" -o jsonpath='{.status.availableReplicas}' 2>/dev/null || true)"
  [[ -n "$n" && "$n" != "0" ]]
}

# Deadline-bounded wait for the UI Deployment: polls the CMSC conditions for
# visibility, up to the deadline; on success prints the UI URL (and opens the
# Route when asked); a FAILED rollout returns 1 immediately — never report
# success after a failed rollout (review critical #15: the old
# `wait_for_ui || exit 1` call shape disabled errexit inside this function).
# Credential-free on purpose: callers print login details.
# Args: kubectl namespace cr_name ui_url open_browser [deadline_secs poll_secs]
wait_for_ui() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local cr_name="${3:?CR name required}"
  local ui_url="${4:?UI URL required}"
  local open_browser="${5:-0}"
  local deadline_secs="${6:-900}"
  local poll_secs="${7:-15}"
  local deadline=$((SECONDS + deadline_secs))
  while (( SECONDS < deadline )); do
    "$kubectl" -n "$namespace" get cmsc "$cr_name" -o jsonpath='phase={.status.phase}{"\n"}{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}' 2>/dev/null || true
    if deploy_available "$kubectl" "$namespace" "${cr_name}-ui"; then
      if ! "$kubectl" -n "$namespace" rollout status "deployment/${cr_name}-ui" --timeout=120s; then
        echo "error: UI Deployment ${cr_name}-ui rollout failed" >&2
        return 1
      fi
      echo "UI ready: ${ui_url}"
      if [[ "$open_browser" == "1" ]] && command -v open >/dev/null 2>&1; then
        open "$ui_url" || true
      fi
      return 0
    fi
    sleep "$poll_secs"
  done
  echo "error: UI Deployment ${cr_name}-ui did not become Available within $((deadline_secs / 60))m" >&2
  "$kubectl" -n "$namespace" get cmsc "$cr_name" -o yaml 2>/dev/null | tail -80 >&2 || true
  return 1
}

# Fixture test hooks must never activate in a production install: the caller
# must set DEMO_ALLOW_FIXTURES=1 explicitly (the offline test runner does).
fixtures_allowed() {
  if [[ "${DEMO_ALLOW_FIXTURES:-}" != "1" ]]; then
    echo "error: ${1} is set, but fixture mode is not enabled. Fixture hooks are" >&2
    echo "  for the offline test suite only (set DEMO_ALLOW_FIXTURES=1 there); in a" >&2
    echo "  real install they would silently fake version/architecture results." >&2
    return 1
  fi
}

# Validate a --version hint before it is interpolated into anything (OLM YAML,
# registry URLs): a tag of v?[0-9A-Za-z._-] plus an optional @sha256:<64 hex>
# digest suffix. Anything else — newlines, quotes, YAML — is rejected outright
# (review warning #17: unvalidated input reached a cluster-admin oc apply).
validate_version_hint() {
  local hint="${1:-}"
  [[ -z "$hint" ]] && return 0
  local tag="${hint%%@*}"
  local digest="${hint#*@}"
  local re_tag='^v?[0-9A-Za-z._-]+$'
  local re_digest='^sha256:[0-9a-f]{64}$'
  if [[ "$hint" == *"@"* && ! "$digest" =~ $re_digest ]]; then
    echo "error: version digest suffix must be @sha256:<64 lowercase hex> (got '@${digest}')" >&2
    return 1
  fi
  if [[ ! "$tag" =~ $re_tag ]]; then
    echo "error: VERSION must be a tag (vX.Y.Z, commit SHA, latest) with an optional @sha256 digest (got '${tag}')" >&2
    return 1
  fi
  return 0
}

# Print the manifest digest of a tag (empty when unknown). Closes the
# tag→pull TOCTOU: the resolved version is pinned as tag@sha256:<digest> so
# OLM pulls exactly what was resolved (review warning #19). Fixture lines may
# carry "tag sha256:<digest>" pairs; lines without a digest are fine.
registry_manifest_digest() {
  local repo="${1:?repository path required}"
  local tag="${2:?tag required}"
  if [[ -n "${REGISTRY_TAG_FIXTURE:-}" ]]; then
    fixtures_allowed REGISTRY_TAG_FIXTURE || return 1
    awk -v t="$tag" '$1 == t && NF >= 2 { print $2; exit }' "$REGISTRY_TAG_FIXTURE" 2>/dev/null || true
    return 0
  fi
  local out
  out="$(_registry_client manifest-digest "$repo" "$tag" 2>/dev/null)" || return 0
  printf '%s\n' "$out"
}

# Print tag@sha256:<digest> — the user-supplied suffix verbatim when given,
# otherwise the digest discovered from the registry (bare tag when unknown).
pin_tag_digest() {
  local repo="${1:?repository path required}"
  local tag="${2:?tag required}"
  local suffix="${3:-}"
  if [[ -n "$suffix" ]]; then
    printf '%s%s\n' "$tag" "$suffix"
    return 0
  fi
  local d
  d="$(registry_manifest_digest "$repo" "$tag")"
  if [[ -n "$d" ]]; then
    printf '%s@%s\n' "$tag" "$d"
  else
    printf '%s\n' "$tag"
  fi
}

# Echo 1 when the browser should open at UI-ready: both the documented
# --no-open flag (DEMO_NO_OPEN) and the OPEN_BROWSER env must allow it
# (re-review regression: the parameterized wait_for_ui had lost DEMO_NO_OPEN).
should_open_browser() {
  if [[ "${DEMO_NO_OPEN:-0}" != "1" && "${OPEN_BROWSER:-1}" == "1" ]]; then
    echo 1
  else
    echo 0
  fi
}

# True when the UI OAuth client Secret must be (re-)mirrored from Keycloak:
# absent, or present but stale because Keycloak is being (re)provisioned this
# run — a fresh Keycloak issues a new client secret, so the previously
# mirrored one cannot authenticate (cross-model review follow-up #25).
oauth_mirror_needed() {
  local kubectl="${1:?kubectl/oc binary}"
  local namespace="${2:?namespace required}"
  local cr_name="${3:?CR name required}"
  local keycloak_skipped="${4:?0 or 1}"
  if ! "$kubectl" -n "$namespace" get secret "${cr_name}-ui-oauth-client" >/dev/null 2>&1; then
    return 0
  fi
  # Secret exists: still needed when Keycloak is being (re)provisioned.
  [[ "$keycloak_skipped" != "1" ]]
}
