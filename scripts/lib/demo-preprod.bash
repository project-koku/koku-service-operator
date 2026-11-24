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
#   DEMO_ENVOY_IMAGE / DEMO_ENVOY_TAG            envoy / proxyv2
#   DEMO_OAUTHPROXY_IMAGE / DEMO_OAUTHPROXY_TAG  oauth2-proxy sidecar
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
text = text.replace('  name: cost-management\n  namespace: cost-byoi\n', f'  name: {name}\n  namespace: {namespace}\n', 1)
text = text.replace('clusterDomain: "apps.cluster.example.com"', f'clusterDomain: "{domain}"')
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
    ("DEMO_ENVOY_IMAGE",      "repository: registry.redhat.io/openshift-service-mesh/proxyv2-rhel9"),
    ("DEMO_ENVOY_TAG",        'tag: "2.6"'),
    ("DEMO_OAUTHPROXY_IMAGE", "repository: registry.redhat.io/rhceph/oauth2-proxy-rhel9"),
    ("DEMO_OAUTHPROXY_TAG",   'tag: "v7.6.0"'),
]
for env_key, needle in _overrides:
    val = os.environ.get(env_key, "").strip()
    if not val:
        continue
    if needle.startswith("repository: "):
        repl = "repository: " + val
    else:
        repl = 'tag: "' + val + '"'
    if needle not in text:
        raise SystemExit(f"render_preprod_cr: {env_key} set but sample line not found: {needle}")
    text = text.replace(needle, repl)

Path(dest).write_text(text)
PY
}

# Fail with the real oc/kubectl error when the API is down (expired Cluster Bot).
require_reachable_cluster() {
  local kubectl="${1:?kubectl/oc binary}"
  local err
  err="$(mktemp)"
  if ! "$kubectl" whoami >/dev/null 2>"$err"; then
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

# List the tags of a public quay repository, one per line (paginated).
# REGISTRY_TAG_FIXTURE: when set, read tags from that file instead of HTTP —
# offline tests; production leaves it unset.
registry_list_tags() {
  local repo="${1:?repository path required, e.g. project-koku/koku-service-operator-catalog}"
  if [[ -n "${REGISTRY_TAG_FIXTURE:-}" ]]; then
    if [[ ! -f "$REGISTRY_TAG_FIXTURE" ]]; then
      echo "error: REGISTRY_TAG_FIXTURE not found: ${REGISTRY_TAG_FIXTURE}" >&2
      return 1
    fi
    cat "$REGISTRY_TAG_FIXTURE"
    return 0
  fi
  local out
  if ! out="$(python3 - "$repo" 2>/dev/null <<'PY'
import json, sys, urllib.request

repo = sys.argv[1]
tags, page = [], 1
while True:
    url = f"https://quay.io/api/v1/repository/{repo}/tag/?limit=100&page={page}&onlyActiveTags=true"
    with urllib.request.urlopen(url, timeout=30) as r:
        d = json.load(r)
    tags += [t["name"] for t in d.get("tags", [])]
    if not d.get("has_additional") or page >= 20:
        break
    page += 1
print("\n".join(tags))
PY
)"; then
    echo "error: failed to list tags of quay.io/${repo} (registry unreachable or API error)" >&2
    return 1
  fi
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
vs = [l.strip() for l in sys.stdin if re.fullmatch(r"v?\d+\.\d+\.\d+", l.strip())]
print(max(vs, key=lambda v: tuple(int(x) for x in v.lstrip("v").split("."))) if vs else "")
')"
    if [[ -n "$best" ]]; then
      printf '%s\n' "$best"
      return 0
    fi
    warn="no semver tag found in quay.io/${repo}; falling back to :latest (a moving target — pin --version for reproducibility)"
    echo "warning: ${warn}" >&2
    printf 'latest\n'
    return 0
  fi

  if [[ "$tag" == "latest" || "$tag" == "main" ]]; then
    printf '%s\n' "$tag"
    return 0
  fi

  if [[ "$tags_ok" != "1" ]]; then
    warn="could not verify '${tag}' against quay.io/${repo} (registry unreachable); using it as-is"
    echo "warning: ${warn}" >&2
    printf '%s%s\n' "$tag" "$digest"
    return 0
  fi

  if printf '%s\n' "$tags" | grep -Fxq "$tag"; then
    printf '%s%s\n' "$tag" "$digest"
    return 0
  fi
  if printf '%s\n' "$tags" | grep -Fxq "v${tag}"; then
    printf 'v%s%s\n' "$tag" "$digest"
    return 0
  fi
  if [[ "$tag" =~ ^[0-9a-f]{7,40}$ ]]; then
    local m
    m="$(printf '%s\n' "$tags" | awk -v h="$tag" 'index(h, $0) == 1 || index($0, h) == 1' | head -1)"
    if [[ -n "$m" ]]; then
      printf '%s%s\n' "$m" "$digest"
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
# clusters warn and use the first node; anything else is unsupported (exit 1).
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
    echo "warning: mixed-architecture cluster ($(printf '%s' "$archs" | tr '\n' ' ')); using ${first}" >&2
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
  out="$(python3 - "$ref" 2>/dev/null <<'PY'
import json, re, sys, urllib.request, urllib.error

ref = sys.argv[1]
if "://" in ref:
    ref = ref.split("://", 1)[1]
registry, rest = ref.split("/", 1)
# docker.io redirects to registry-1.docker.io and urllib drops the auth header
# across hosts — talk to the real registry directly.
if registry in ("docker.io", "index.docker.io"):
    registry = "registry-1.docker.io"
if "@" in rest:
    repo, _, reference = rest.rpartition("@")
    # A pod-spec image may carry both tag and digest (repo:tag@sha256:...).
    base_repo, _, maybe_tag = repo.rpartition(":")
    if "/" not in maybe_tag:
        repo = base_repo
else:
    repo, _, tag = rest.rpartition(":")
    if "/" in tag or not tag:
        repo, reference = rest, "latest"
    else:
        reference = tag
base = f"https://{registry}"

def get(url, token=None, accept=None):
    req = urllib.request.Request(url)
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    if accept:
        req.add_header("Accept", accept)
    with urllib.request.urlopen(req, timeout=30) as r:
        return r.read()

token = ""
try:
    get(base + "/v2/")
except urllib.error.HTTPError as e:
    www = e.headers.get("WWW-Authenticate", "").replace('",', '" ')
    opts = dict(re.findall(r'(\w+)="([^"]*)"', www))
    realm = opts.get("realm")
    if realm:
        q = f"?service={opts.get('service', '')}"
        q += f"&scope=repository:{repo}:pull"
        try:
            tok = json.loads(get(realm + q))
            token = tok.get("token") or tok.get("access_token") or ""
        except Exception:
            token = ""
except Exception:
    pass

accept = ", ".join([
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
])
try:
    m = json.loads(get(f"{base}/v2/{repo}/manifests/{reference}", token, accept))
except Exception:
    print("unknown"); raise SystemExit
if "manifests" in m:
    archs = set()
    for man in m["manifests"]:
        p = man.get("platform", {})
        if p.get("os", "linux") == "linux":
            archs.add(p.get("architecture", "unknown"))
    print(" ".join(sorted(archs)) or "unknown")
else:
    cfg = m.get("config", {}).get("digest")
    if cfg:
        try:
            c = json.loads(get(f"{base}/v2/{repo}/blobs/{cfg}", token))
            print(c.get("architecture", "unknown"))
        except Exception:
            print("unknown")
    else:
        print("unknown")
PY
)" || out="unknown"
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
