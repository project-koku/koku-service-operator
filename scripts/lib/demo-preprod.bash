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
